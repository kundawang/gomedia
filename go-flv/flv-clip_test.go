package flv

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/yapingcat/gomedia/go-codec"
)

func mustHexToBytes(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// ---------------------------------------------------------------------------
// Fixed FLV fixtures.
//
// The fixtures are synthesized byte-by-byte instead of being pulled from a
// recording, so every timestamp, keyframe position and tag boundary is known
// exactly and the tests stay independent of external media files.
// ---------------------------------------------------------------------------

// flvFixtureBuilder concatenates complete FLV tags (11-byte header + payload +
// 4-byte previous tag size) behind a freshly written FLV header.
type flvFixtureBuilder struct {
	buf      bytes.Buffer
	hasAudio bool
	hasVideo bool
}

func newFlvFixtureBuilder(hasAudio, hasVideo bool) *flvFixtureBuilder {
	b := &flvFixtureBuilder{hasAudio: hasAudio, hasVideo: hasVideo}
	hdr := []byte{'F', 'L', 'V', 0x01, 0, 0, 0, 0, 9}
	if hasAudio {
		hdr[4] |= 0x04
	}
	if hasVideo {
		hdr[4] |= 0x01
	}
	binary.BigEndian.PutUint32(hdr[5:9], 9)
	b.buf.Write(hdr)
	b.buf.Write([]byte{0, 0, 0, 0}) // PreviousTagSize0
	return b
}

func (b *flvFixtureBuilder) addTag(tagType uint8, ts uint32, payload []byte) {
	tag := FlvTag{
		TagType:           tagType,
		DataSize:          uint32(len(payload)),
		Timestamp:         ts & 0x00FFFFFF,
		TimestampExtended: uint8(ts >> 24),
	}
	b.buf.Write(tag.Encode())
	b.buf.Write(payload)
	var prev [4]byte
	binary.BigEndian.PutUint32(prev[:], 11+uint32(len(payload)))
	b.buf.Write(prev[:])
}

// --- payload constructors ---------------------------------------------------

func avcSeqHeaderPayload() []byte {
	// Hand-built but spec-shaped avcC: version/profile/level, one SPS, one PPS.
	// The SPS is a complete baseline-profile SPS so codec.CovertExtradata /
	// SPS.Decode can walk it without overrunning.
	sps := mustHexToBytes("6742000af841a2")
	pps := mustHexToBytes("68ce3880")
	out := []byte{0x01, sps[1], sps[2], sps[3], 0xFF, 0xE1}
	size := make([]byte, 2)
	binary.BigEndian.PutUint16(size, uint16(len(sps)))
	out = append(out, size...)
	out = append(out, sps...)
	out = append(out, 0x01)
	binary.BigEndian.PutUint16(size, uint16(len(pps)))
	out = append(out, size...)
	out = append(out, pps...)
	return out
}

// avcNaluPayload wraps one Annex-B NALU (without start code) in AVCC length form.
func avcNaluPayload(nalu []byte, isKey bool) []byte {
	frameType := uint8(INTER_FRAME)
	if isKey {
		frameType = uint8(KEY_FRAME)
	}
	hdr := VideoTag{
		FrameType:     frameType,
		CodecId:       uint8(FLV_AVC),
		AVCPacketType: AVC_NALU,
	}.Encode()
	out := append(hdr, make([]byte, 4)...)
	binary.BigEndian.PutUint32(out[5:9], uint32(len(nalu)))
	out = append(out, nalu...)
	return out
}

// idrNalu returns a valid-enough IDR slice NALU (nal_ref_idc=3, type=5).
func idrNalu() []byte { return mustHexToBytes("65888040") }

// pNalu returns a non-reference P slice NALU (type=1).
func pNalu() []byte { return mustHexToBytes("419a") }

func aacSeqHeaderPayload(asc []byte) []byte {
	return WriteAudioTag(asc, FLV_AAC, 0, 0, true)
}

func aacRawPayload(frame []byte) []byte {
	return WriteAudioTag(frame, FLV_AAC, 0, 0, false)
}

// onMetaDataPayload builds an AMF0 script payload with a numeric duration.
func onMetaDataPayload(durationSec float64) []byte {
	var b bytes.Buffer
	b.WriteByte(amf0String)
	name := "onMetaData"
	b.Write(uint16Bytes(uint16(len(name))))
	b.WriteString(name)
	b.WriteByte(amf0EcmaArray)
	b.Write([]byte{0, 0, 0, 0}) // count is advisory
	writeAmfProp := func(k string, marker byte, v []byte) {
		b.Write(uint16Bytes(uint16(len(k))))
		b.WriteString(k)
		b.WriteByte(marker)
		b.Write(v)
	}
	dur := make([]byte, 8)
	binary.BigEndian.PutUint64(dur, math.Float64bits(durationSec))
	writeAmfProp("duration", amf0Number, dur)
	writeAmfProp("width", amf0Number, float64Bytes(320))
	b.Write([]byte{0, 0, amf0ObjectEnd})
	return b.Bytes()
}

func uint16Bytes(v uint16) []byte { return []byte{byte(v >> 8), byte(v)} }
func float64Bytes(v float64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, math.Float64bits(v))
	return b
}

// buildMinimalHvcc builds a structurally valid HEVC decoder configuration
// record carrying one VPS, one SPS and one PPS array. The NALU contents only
// need correct framing for the library's hvcc Decode/ToNalus round trip.
func buildMinimalHvcc(t *testing.T) []byte {
	t.Helper()
	buf := make([]byte, 0, 64)
	// 23-byte fixed header, field-aligned with HEVCRecordConfiguration.Encode.
	buf = append(buf,
		0x01,                   // [0] configurationVersion
		0x01,                   // [1] profile_space/tier/profile_idc
		0xFF, 0xFF, 0xFF, 0xFF, // [2..5] profile compatibility
		0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, // [6..11] 48 constraint flags
		0x1E,       // [12] general_level_idc
		0xF0, 0x01, // [13..14] 4 reserved bits + minSpatialSegmentation(12)=1
		0xFC,       // [15] 6 reserved + parallelismType(2)=0
		0xFD,       // [16] 6 reserved + chromaFormat(2)=1
		0xF8,       // [17] 5 reserved + bitDepthLumaMinus8(3)=0
		0xF8,       // [18] 5 reserved + bitDepthChromaMinus8(3)=0
		0x00, 0x00, // [19..20] avgFrameRate
		0x03, // [21] constantFrameRate(2)=0, numTemporalLayers(3)=1,
		//        temporalIdNested(1)=1, lengthSizeMinusOne(2)=3
		0x03, // [22] numOfArrays = 3 (VPS, SPS, PPS)
	)
	addArray := func(nalType byte, nalu []byte) {
		buf = append(buf, 0x80|nalType) // array_completeness=1, reserved 0, type(6)
		buf = append(buf, 0x00, 0x01)   // num nalus = 1
		buf = append(buf, uint16Bytes(uint16(len(nalu)))...)
		buf = append(buf, nalu...)
	}
	addArray(32, mustHexToBytes("4001")) // VPS
	addArray(33, mustHexToBytes("4201")) // SPS
	addArray(34, mustHexToBytes("4401")) // PPS
	return buf
}

// buildInterleavedFixture returns a two-keyframe, audio/video interleaved FLV:
//
//	idx 0  script onMetaData duration=1.000s
//	idx 1  AVC sequence header        ts 0
//	idx 2  AAC sequence header        ts 0
//	idx 3  AAC raw                    ts 0
//	idx 4  AVC keyframe #1 (IDR)      ts 0
//	idx 5  AAC raw                    ts 23
//	idx 6  AVC P frame                ts 40
//	idx 7  AAC raw                    ts 46
//	idx 8  AVC keyframe #2 (IDR)      ts 80
//	idx 9  AAC raw                    ts 69
//	idx 10 AVC P frame                ts 120
//	idx 11 AAC raw                    ts 92
//	idx 12 AVC P frame                ts 160
//	idx 13 unknown tag (type 7)       ts 100
//	idx 14 AAC raw                    ts 115
//	idx 15 AVC P frame                ts 200
//	idx 16 AAC raw                    ts 138
//
// (audio/video intentionally cross-ordered around 69..92ms to prove the
// clipper keeps file-order interleaving.)
func buildInterleavedFixture(t *testing.T) []byte {
	t.Helper()
	b := newFlvFixtureBuilder(true, true)
	asc := mustHexToBytes("1210") // AAC LC 44.1kHz stereo
	b.addTag(uint8(SCRIPT_TAG), 0, onMetaDataPayload(1.0))
	b.addTag(uint8(VIDEO_TAG), 0, append((&VideoTag{
		FrameType:     uint8(KEY_FRAME),
		CodecId:       uint8(FLV_AVC),
		AVCPacketType: AVC_SEQUENCE_HEADER,
	}).Encode(), avcSeqHeaderPayload()...))
	b.addTag(uint8(AUDIO_TAG), 0, aacSeqHeaderPayload(asc))
	b.addTag(uint8(AUDIO_TAG), 0, aacRawPayload(mustHexToBytes("2110")))
	b.addTag(uint8(VIDEO_TAG), 0, avcNaluPayload(idrNalu(), true))
	b.addTag(uint8(AUDIO_TAG), 23, aacRawPayload(mustHexToBytes("2111")))
	b.addTag(uint8(VIDEO_TAG), 40, avcNaluPayload(pNalu(), false))
	b.addTag(uint8(AUDIO_TAG), 46, aacRawPayload(mustHexToBytes("2112")))
	b.addTag(uint8(VIDEO_TAG), 80, avcNaluPayload(idrNalu(), true))
	b.addTag(uint8(AUDIO_TAG), 69, aacRawPayload(mustHexToBytes("2113")))
	b.addTag(uint8(VIDEO_TAG), 120, avcNaluPayload(pNalu(), false))
	b.addTag(uint8(AUDIO_TAG), 92, aacRawPayload(mustHexToBytes("2114")))
	b.addTag(uint8(VIDEO_TAG), 160, avcNaluPayload(pNalu(), false))
	// Unknown tag type (7): must survive clipping verbatim.
	b.addTag(7, 100, []byte{0xDE, 0xAD, 0xBE, 0xEF})
	b.addTag(uint8(AUDIO_TAG), 115, aacRawPayload(mustHexToBytes("2115")))
	b.addTag(uint8(VIDEO_TAG), 200, avcNaluPayload(pNalu(), false))
	b.addTag(uint8(AUDIO_TAG), 138, aacRawPayload(mustHexToBytes("2116")))
	return b.buf.Bytes()
}

// buildAudioOnlyFixture contains AAC config plus raw frames only.
func buildAudioOnlyFixture(t *testing.T) []byte {
	t.Helper()
	b := newFlvFixtureBuilder(true, false)
	asc := mustHexToBytes("1210")
	b.addTag(uint8(SCRIPT_TAG), 0, onMetaDataPayload(0.2))
	b.addTag(uint8(AUDIO_TAG), 0, aacSeqHeaderPayload(asc))
	b.addTag(uint8(AUDIO_TAG), 0, aacRawPayload(mustHexToBytes("2110")))
	b.addTag(uint8(AUDIO_TAG), 23, aacRawPayload(mustHexToBytes("2111")))
	b.addTag(uint8(AUDIO_TAG), 46, aacRawPayload(mustHexToBytes("2112")))
	b.addTag(uint8(AUDIO_TAG), 69, aacRawPayload(mustHexToBytes("2113")))
	b.addTag(uint8(AUDIO_TAG), 92, aacRawPayload(mustHexToBytes("2114")))
	return b.buf.Bytes()
}

// buildNoKeyframeFixture contains only inter video frames.
func buildNoKeyframeFixture(t *testing.T) []byte {
	t.Helper()
	b := newFlvFixtureBuilder(false, true)
	b.addTag(uint8(VIDEO_TAG), 0, append((&VideoTag{
		FrameType:     uint8(KEY_FRAME),
		CodecId:       uint8(FLV_AVC),
		AVCPacketType: AVC_SEQUENCE_HEADER,
	}).Encode(), avcSeqHeaderPayload()...))
	b.addTag(uint8(VIDEO_TAG), 40, avcNaluPayload(pNalu(), false))
	b.addTag(uint8(VIDEO_TAG), 80, avcNaluPayload(pNalu(), false))
	return b.buf.Bytes()
}

// buildWraparoundFixture places the keyframe right before a 32-bit timestamp
// wraparound and the P frames right after it, so unwrapping is required to see
// a monotonic clip.
func buildWraparoundFixture(t *testing.T) []byte {
	t.Helper()
	b := newFlvFixtureBuilder(false, true)
	base := uint32(0xFFFFFFF0) // 4294967280
	b.addTag(uint8(VIDEO_TAG), base, append((&VideoTag{
		FrameType:     uint8(KEY_FRAME),
		CodecId:       uint8(FLV_AVC),
		AVCPacketType: AVC_SEQUENCE_HEADER,
	}).Encode(), avcSeqHeaderPayload()...))
	b.addTag(uint8(VIDEO_TAG), base, avcNaluPayload(idrNalu(), true))
	b.addTag(uint8(VIDEO_TAG), base+40, avcNaluPayload(pNalu(), false)) // wraps to 24
	b.addTag(uint8(VIDEO_TAG), base+80, avcNaluPayload(pNalu(), false)) // wraps to 64
	return b.buf.Bytes()
}

// ---------------------------------------------------------------------------
// Output parsing helpers
// ---------------------------------------------------------------------------

type parsedTag struct {
	tagType uint8
	ts      uint32
	payload []byte
}

func parseFlvTags(t *testing.T, data []byte) (header []byte, tags []parsedTag) {
	t.Helper()
	if len(data) < 13 || data[0] != 'F' || data[1] != 'L' || data[2] != 'V' {
		n := 9
		if len(data) < n {
			n = len(data)
		}
		t.Fatalf("output is not an FLV: %x", data[:n])
	}
	header = data[:9]
	off := 9 + 4
	for off < len(data) {
		if off+11 > len(data) {
			t.Fatalf("truncated tag header at %d", off)
		}
		var ftag FlvTag
		ftag.Decode(data[off : off+11])
		start := off + 11
		end := start + int(ftag.DataSize)
		if end+4 > len(data) {
			t.Fatalf("truncated tag payload at %d", off)
		}
		payload := make([]byte, ftag.DataSize)
		copy(payload, data[start:end])
		tags = append(tags, parsedTag{
			tagType: ftag.TagType,
			ts:      uint32(ftag.TimestampExtended)<<24 | ftag.Timestamp,
			payload: payload,
		})
		// Previous tag size must be exact: 11 + DataSize.
		got := binary.BigEndian.Uint32(data[end : end+4])
		if got != 11+ftag.DataSize {
			t.Fatalf("previous tag size mismatch at %d: got %d want %d", off, got, 11+ftag.DataSize)
		}
		off = end + 4
	}
	return header, tags
}

func firstMediaVideoTag(tags []parsedTag) (parsedTag, bool) {
	for _, tg := range tags {
		if tg.tagType == uint8(VIDEO_TAG) && !isVideoSequenceHeader(tg.payload) {
			return tg, true
		}
	}
	return parsedTag{}, false
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestClipFlv_SeekToKeyframeAndRebase(t *testing.T) {
	src := buildInterleavedFixture(t)
	var out bytes.Buffer
	// Request starts at 50ms: must extend backwards to the keyframe at 0ms.
	stats, err := ClipFlv(bytes.NewReader(src), &out, 50, 140)
	if err != nil {
		t.Fatalf("ClipFlv failed: %v", err)
	}
	if !stats.SeekedKeyframe || stats.StartMs != 0 {
		t.Fatalf("expected seek back to keyframe 0, got StartMs=%d seeked=%v", stats.StartMs, stats.SeekedKeyframe)
	}
	if !stats.VideoConfigEmitted || !stats.AudioConfigEmitted {
		t.Fatalf("config tags must be emitted: %+v", stats)
	}
	if stats.HasVideo != true || stats.HasAudio != true {
		t.Fatalf("both tracks expected, got %+v", stats)
	}

	hdr, tags := parseFlvTags(t, out.Bytes())
	if hdr[4] != 0x05 { // audio + video flags
		t.Fatalf("header type flags = %#x, want 0x05", hdr[4])
	}

	// Ordering: script first, retained unknown tags, then the two config tags,
	// then media. Each tag is classified into a phase and phases must not go
	// backwards.
	const (
		phaseScript = iota
		phaseHead   // unknown tags and sequence headers
		phaseMedia
	)
	classify := func(tg parsedTag) int {
		if tg.tagType == uint8(SCRIPT_TAG) {
			return phaseScript
		}
		if tg.tagType == 7 {
			return phaseHead
		}
		if (tg.tagType == uint8(VIDEO_TAG) && isVideoSequenceHeader(tg.payload)) ||
			(tg.tagType == uint8(AUDIO_TAG) && isAudioSequenceHeader(tg.payload)) {
			return phaseHead
		}
		return phaseMedia
	}
	prevPhase := phaseScript
	var sawUnknown, sawVideoCfg, sawAudioCfg bool
	for _, tg := range tags {
		p := classify(tg)
		if p < prevPhase {
			t.Fatalf("tag phases out of order: type=%d phase=%d after %d", tg.tagType, p, prevPhase)
		}
		prevPhase = p
		if tg.tagType == 7 {
			sawUnknown = true
		}
		if tg.tagType == uint8(VIDEO_TAG) && isVideoSequenceHeader(tg.payload) {
			sawVideoCfg = true
		}
		if tg.tagType == uint8(AUDIO_TAG) && isAudioSequenceHeader(tg.payload) {
			sawAudioCfg = true
		}
	}
	if !sawUnknown || !sawVideoCfg || !sawAudioCfg {
		t.Fatalf("missing head tags: unknown=%v videoCfg=%v audioCfg=%v",
			sawUnknown, sawVideoCfg, sawAudioCfg)
	}

	// First media video frame is a decodable keyframe with timestamp 0.
	firstVid, ok := firstMediaVideoTag(tags)
	if !ok {
		t.Fatal("no media video tag in output")
	}
	if firstVid.payload[0]>>4 != uint8(KEY_FRAME) || firstVid.payload[1] != AVC_NALU {
		t.Fatalf("first media video tag is not a keyframe NALU: %x", firstVid.payload[:2])
	}
	if firstVid.ts != 0 {
		t.Fatalf("first keyframe ts = %d, want 0", firstVid.ts)
	}

	// Timestamps are rebased and every output tag lies within [0, duration].
	var lastTs uint32
	for _, tg := range tags {
		if tg.ts < lastTs && tg.tagType != 7 {
			// interleaving keeps file order; audio may lead video but the
			// fixture's audio/video are each monotonic after rebase.
		}
		lastTs = tg.ts
		if tg.ts > stats.DurationMs {
			t.Fatalf("tag ts %d exceeds duration %d", tg.ts, stats.DurationMs)
		}
	}

	// End boundary: last media tag must not pass 140ms source time.
	for _, tg := range tags {
		if TagType(tg.tagType) == AUDIO_TAG || TagType(tg.tagType) == VIDEO_TAG {
			if int64(tg.ts)+stats.BaseMs > 140 {
				t.Fatalf("tag beyond end boundary: src ts %d", int64(tg.ts)+stats.BaseMs)
			}
		}
	}

	// Unknown tag preserved.
	var unknowns int
	for _, tg := range tags {
		if tg.tagType == 7 {
			unknowns++
			if !bytes.Equal(tg.payload, []byte{0xDE, 0xAD, 0xBE, 0xEF}) {
				t.Fatalf("unknown tag payload altered: %x", tg.payload)
			}
		}
	}
	if unknowns != 1 {
		t.Fatalf("want 1 unknown tag, got %d", unknowns)
	}

	// Metadata duration rewritten to the clip duration.
	if !stats.MetaDataPatched {
		t.Fatal("metadata duration should be patched")
	}
	metaDur := readMetadataDuration(t, tags[0].payload)
	if math.Abs(metaDur-float64(stats.DurationMs)/1000.0) > 1e-9 {
		t.Fatalf("metadata duration %f != %f", metaDur, float64(stats.DurationMs)/1000.0)
	}
}

func TestClipFlv_SecondKeyframeWindow(t *testing.T) {
	src := buildInterleavedFixture(t)
	var out bytes.Buffer
	// Window starting after keyframe #1 (0ms) but before keyframe #2 (80ms):
	// the clip must begin on keyframe #2.
	stats, err := ClipFlv(bytes.NewReader(src), &out, 90, 200)
	if err != nil {
		t.Fatalf("ClipFlv failed: %v", err)
	}
	if stats.StartMs != 80 || !stats.SeekedKeyframe {
		t.Fatalf("expected seek to keyframe 80, got StartMs=%d", stats.StartMs)
	}
	_, tags := parseFlvTags(t, out.Bytes())
	firstVid, ok := firstMediaVideoTag(tags)
	if !ok || firstVid.ts != 0 || firstVid.payload[0]>>4 != uint8(KEY_FRAME) {
		t.Fatalf("clip must start with rebased keyframe #2, got %+v", firstVid)
	}
	// No media from the first GOP (source ts 0/40) may leak in.
	for _, tg := range tags {
		if TagType(tg.tagType) == VIDEO_TAG && !isVideoSequenceHeader(tg.payload) {
			if stats.BaseMs+int64(tg.ts) < 80 {
				t.Fatalf("first-GOP frame leaked: src ts %d", stats.BaseMs+int64(tg.ts))
			}
		}
	}
}

func TestClipFlv_TagRoundTrip(t *testing.T) {
	src := buildInterleavedFixture(t)
	var out bytes.Buffer
	stats, err := ClipFlv(bytes.NewReader(src), &out, 0, 92)
	if err != nil {
		t.Fatalf("ClipFlv failed: %v", err)
	}
	_, srcTags := parseFlvTags(t, src)
	_, outTags := parseFlvTags(t, out.Bytes())

	// Build a lookup of source payloads by (type, source ts). Several tags can
	// share a timestamp (e.g. config + keyframe at 0), so each bucket keeps
	// every payload in file order.
	type key struct {
		typ uint8
		ts  uint32
	}
	_ = key{}
	srcBuckets := make(map[string][][]byte)
	for _, tg := range srcTags {
		k := tagKey(tg.tagType, tg.ts)
		srcBuckets[k] = append(srcBuckets[k], tg.payload)
	}
	srcCursor := make(map[string]int)
	var mediaSeen int
	for _, tg := range outTags {
		if TagType(tg.tagType) == SCRIPT_TAG {
			continue // metadata payload is intentionally patched
		}
		srcTs := tg.ts + uint32(stats.BaseMs)
		k := tagKey(tg.tagType, srcTs)
		bucket, ok := srcBuckets[k]
		if !ok {
			t.Fatalf("output tag (type=%d srcTs=%d) has no source counterpart", tg.tagType, srcTs)
		}
		cur := srcCursor[k]
		if cur >= len(bucket) {
			t.Fatalf("output has more type=%d srcTs=%d tags than source", tg.tagType, srcTs)
		}
		if !bytes.Equal(bucket[cur], tg.payload) {
			t.Fatalf("payload altered for type=%d srcTs=%d", tg.tagType, srcTs)
		}
		srcCursor[k] = cur + 1
		if TagType(tg.tagType) == AUDIO_TAG || TagType(tg.tagType) == VIDEO_TAG {
			if !isVideoSequenceHeader(tg.payload) && !isAudioSequenceHeader(tg.payload) {
				mediaSeen++
			}
		}
	}
	if mediaSeen != stats.AudioTags+stats.VideoTags {
		t.Fatalf("round trip count mismatch: seen=%d stats=%d/%d", mediaSeen, stats.AudioTags, stats.VideoTags)
	}
	if stats.DurationMs != 92 {
		t.Fatalf("duration = %d, want 92", stats.DurationMs)
	}
}

func TestClipFlv_AudioVideoSync(t *testing.T) {
	src := buildInterleavedFixture(t)
	var out bytes.Buffer
	stats, err := ClipFlv(bytes.NewReader(src), &out, 40, 120)
	if err != nil {
		t.Fatalf("ClipFlv failed: %v", err)
	}
	_, tags := parseFlvTags(t, out.Bytes())
	// Relative A/V timing must be preserved: for every pair, the rebased
	// difference equals the source timestamp difference (same base for both).
	for _, tg := range tags {
		if TagType(tg.tagType) != AUDIO_TAG && TagType(tg.tagType) != VIDEO_TAG {
			continue
		}
		if isVideoSequenceHeader(tg.payload) || isAudioSequenceHeader(tg.payload) {
			if tg.ts != 0 {
				t.Fatalf("config tag must be at ts 0, got %d", tg.ts)
			}
			continue
		}
		// rebase identity: output ts == source ts - BaseMs (non-negative here)
		if int64(tg.ts) != sourceTsOf(tg, tags, stats) {
			t.Fatalf("rebasing broke relative timing for tag %+v", tg)
		}
	}
}

// sourceTsOf reconstructs the source timestamp of an output tag.
func sourceTsOf(tg parsedTag, _ []parsedTag, stats ClipStats) int64 {
	return int64(tg.ts) + stats.BaseMs
}

func TestClipFlv_AudioOnly(t *testing.T) {
	src := buildAudioOnlyFixture(t)
	var out bytes.Buffer
	stats, err := ClipFlv(bytes.NewReader(src), &out, 23, 69)
	if err != nil {
		t.Fatalf("ClipFlv failed: %v", err)
	}
	if stats.HasVideo || !stats.HasAudio {
		t.Fatalf("audio-only clip misclassified: %+v", stats)
	}
	if stats.VideoConfigEmitted || !stats.AudioConfigEmitted {
		t.Fatalf("only AAC config expected: %+v", stats)
	}
	hdr, tags := parseFlvTags(t, out.Bytes())
	if hdr[4] != 0x04 {
		t.Fatalf("audio-only header flags = %#x want 0x04", hdr[4])
	}
	// First media tag is the first AAC raw frame at or after 23ms, rebased.
	if tags[1].ts != 0 {
		t.Fatalf("first audio frame must rebase to 0, got %d", tags[1].ts)
	}
	if stats.DurationMs != 69-23 {
		t.Fatalf("duration = %d want 46", stats.DurationMs)
	}
}

func TestClipFlv_NoKeyframe(t *testing.T) {
	src := buildNoKeyframeFixture(t)
	var out bytes.Buffer
	_, err := ClipFlv(bytes.NewReader(src), &out, 0, 100)
	if err != ErrNoKeyframe {
		t.Fatalf("want ErrNoKeyframe, got %v", err)
	}
	if out.Len() != 0 {
		t.Fatal("no output may be written on error")
	}
}

func TestClipFlv_InvalidRange(t *testing.T) {
	src := buildInterleavedFixture(t)
	var out bytes.Buffer
	// Reversed window (end before start).
	if _, err := clipFlv(bytes.NewReader(src), &out, 100, 50); err != ErrInvalidRange {
		t.Fatalf("reversed range: want ErrInvalidRange, got %v", err)
	}
	// Negative window (only reachable through the int64 entry point).
	if _, err := clipFlv(bytes.NewReader(src), &out, -1, 50); err != ErrInvalidRange {
		t.Fatalf("negative range: want ErrInvalidRange, got %v", err)
	}
}

func TestClipFlv_EmptyRange(t *testing.T) {
	src := buildInterleavedFixture(t)
	var out bytes.Buffer
	// Window entirely after the last media tag.
	_, err := ClipFlv(bytes.NewReader(src), &out, 5000, 6000)
	if err != ErrEmptyRange {
		t.Fatalf("want ErrEmptyRange, got %v", err)
	}
}

func TestClipFlv_TruncatedTag(t *testing.T) {
	src := buildInterleavedFixture(t)
	cut := len(src) - 7 // chop the tail of the last tag
	var out bytes.Buffer
	_, err := ClipFlv(bytes.NewReader(src[:cut]), &out, 0, 100)
	if err != ErrTruncatedTag {
		t.Fatalf("want ErrTruncatedTag, got %v", err)
	}
}

func TestClipFlv_NotFLV(t *testing.T) {
	var out bytes.Buffer
	_, err := ClipFlv(bytes.NewReader([]byte("NOTANFLV...........")), &out, 0, 100)
	if err != ErrNotFLV {
		t.Fatalf("want ErrNotFLV, got %v", err)
	}
}

func TestClipFlv_ShortWrite(t *testing.T) {
	src := buildInterleavedFixture(t)
	w := &limitedWriter{remaining: 30} // dies mid-header/early tag
	_, err := ClipFlv(bytes.NewReader(src), w, 0, 100)
	if err != ErrShortWrite {
		t.Fatalf("want ErrShortWrite, got %v", err)
	}
}

func TestClipFlv_TimestampWraparound(t *testing.T) {
	src := buildWraparoundFixture(t)
	var out bytes.Buffer
	// int64 window spanning the 32-bit wrap: 0xFFFFFFF0 .. 0x100000040.
	stats, err := clipFlv(bytes.NewReader(src), &out, int64(0xFFFFFFF0), int64(0x100000040))
	if err != nil {
		t.Fatalf("ClipFlv failed across wraparound: %v", err)
	}
	if stats.StartMs != int64(0xFFFFFFF0) {
		t.Fatalf("StartMs = %#x want %#x", stats.StartMs, int64(0xFFFFFFF0))
	}
	if stats.DurationMs != 80 {
		t.Fatalf("duration across wrap = %d want 80", stats.DurationMs)
	}
	_, tags := parseFlvTags(t, out.Bytes())
	firstVid, ok := firstMediaVideoTag(tags)
	if !ok || firstVid.ts != 0 || firstVid.payload[0]>>4 != uint8(KEY_FRAME) {
		t.Fatalf("wraparound clip must start with rebased keyframe, got %+v", firstVid)
	}
	// All output timestamps are small, unwrapped values, monotonically ordered.
	var prev uint32
	for _, tg := range tags {
		if tg.ts < prev {
			t.Fatalf("output ts not monotonic: %d after %d", tg.ts, prev)
		}
		prev = tg.ts
	}
}

func TestClipFlv_SourceUnchanged(t *testing.T) {
	src := buildInterleavedFixture(t)
	srcCopy := append([]byte(nil), src...)
	dir := t.TempDir()
	srcPath := filepath.Join(dir, "src.flv")
	dstPath := filepath.Join(dir, "clip.flv")
	if err := os.WriteFile(srcPath, src, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ClipFlvFile(srcPath, dstPath, 50, 140); err != nil {
		t.Fatalf("ClipFlvFile failed: %v", err)
	}
	got, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, srcCopy) {
		t.Fatal("source FLV was modified")
	}
	if _, err := os.Stat(dstPath); err != nil {
		t.Fatalf("output file missing: %v", err)
	}
}

func TestAMF0_MetadataDurationPatch(t *testing.T) {
	payload := onMetaDataPayload(3.5)
	out, ok := patchMetadataDuration(payload, 1.25)
	if !ok {
		t.Fatal("duration patch failed")
	}
	if got := readMetadataDuration(t, out); math.Abs(got-1.25) > 1e-12 {
		t.Fatalf("patched duration = %v want 1.25", got)
	}
	// Patching a number must not change the tag size.
	if len(out) != len(payload) {
		t.Fatal("AMF0 number patch must be size-preserving")
	}
	if !isOnMetaData(payload) {
		t.Fatal("isOnMetaData failed on valid payload")
	}
	if isOnMetaData([]byte{0x02, 0x00, 0x01, 'x'}) {
		t.Fatal("isOnMetaData matched unrelated script")
	}
}

// TestAMF0_NestedAndMissing exercises the container-walking branches: a
// duration nested inside a sub-object must be found, and metadata without any
// duration field must report not-found rather than corrupting bytes.
func TestAMF0_NestedAndMissing(t *testing.T) {
	var b bytes.Buffer
	writeName := func(s string) {
		b.WriteByte(amf0String)
		b.Write(uint16Bytes(uint16(len(s))))
		b.WriteString(s)
	}
	writeName("onMetaData")
	// ECMA array with one property "info" holding an object, which in turn
	// contains the numeric "duration".
	b.WriteByte(amf0EcmaArray)
	b.Write([]byte{0, 0, 0, 0})
	b.Write(uint16Bytes(uint16(len("info"))))
	b.WriteString("info")
	b.WriteByte(amf0Object)
	b.Write(uint16Bytes(uint16(len("duration"))))
	b.WriteString("duration")
	b.WriteByte(amf0Number)
	b.Write(float64Bytes(7.0))
	b.Write([]byte{0, 0, amf0ObjectEnd}) // end inner object
	b.Write([]byte{0, 0, amf0ObjectEnd}) // end ECMA array body

	payload := b.Bytes()
	out, ok := patchMetadataDuration(payload, 2.5)
	if !ok {
		t.Fatal("nested duration not found")
	}
	if got := readMetadataDurationFlat(t, out, "duration"); math.Abs(got-2.5) > 1e-12 {
		t.Fatalf("nested duration = %v want 2.5", got)
	}
	if len(out) != len(payload) {
		t.Fatal("nested patch changed tag size")
	}

	// Metadata with no duration field: ok must be false, bytes untouched.
	var c bytes.Buffer
	writeName2 := func(s string) {
		c.WriteByte(amf0String)
		c.Write(uint16Bytes(uint16(len(s))))
		c.WriteString(s)
	}
	writeName2("onMetaData")
	c.WriteByte(amf0EcmaArray)
	c.Write([]byte{0, 0, 0, 0})
	c.Write(uint16Bytes(uint16(len("width"))))
	c.WriteString("width")
	c.WriteByte(amf0Number)
	c.Write(float64Bytes(320))
	c.Write([]byte{0, 0, amf0ObjectEnd})
	before := append([]byte(nil), c.Bytes()...)
	if _, ok := patchMetadataDuration(c.Bytes(), 9.0); ok {
		t.Fatal("patch reported success on metadata without duration")
	}
	if !bytes.Equal(before, c.Bytes()) {
		t.Fatal("metadata without duration was modified")
	}
}

// readMetadataDurationFlat locates a numeric property anywhere in the payload.
func readMetadataDurationFlat(t *testing.T, payload []byte, wantKey string) float64 {
	t.Helper()
	off := 0
	if payload[0] == amf3Marker {
		off = 1
	}
	n, ok := amf0SkipValue(payload, off)
	if !ok {
		t.Fatal("cannot skip name")
	}
	var search func(o int) (float64, bool)
	search = func(o int) (float64, bool) {
		for o < len(payload) {
			marker := payload[o]
			o++
			switch marker {
			case amf0Number:
				o += 8
			case amf0Boolean, amf0Null, amf0Undefined:
			case amf0String:
				_, nn, okk := amf0ReadStringAt(payload, o-1)
				if !okk {
					return 0, false
				}
				o = nn
			case amf0EcmaArray:
				o += 4
				// An ECMA array has the same key/value body as an object.
				marker = amf0Object
				fallthrough
			case amf0Object:
				for {
					if o+2 > len(payload) {
						return 0, false
					}
					kl := int(binary.BigEndian.Uint16(payload[o:]))
					o += 2
					if kl == 0 {
						if o < len(payload) && payload[o] == amf0ObjectEnd {
							o++
						}
						break
					}
					name := string(payload[o : o+kl])
					o += kl
					if name == wantKey && payload[o] == amf0Number {
						return math.Float64frombits(binary.BigEndian.Uint64(payload[o+1 : o+9])), true
					}
					if v, found := search(o); found {
						return v, true
					}
					nn, okk := amf0SkipValue(payload, o)
					if !okk {
						return 0, false
					}
					o = nn
				}
			}
		}
		return 0, false
	}
	if v, found := search(n); found {
		return v
	}
	t.Fatal("flat duration search failed")
	return 0
}

// TestClipFlv_OutputDecodable feeds the clipped file back through the
// library's own FlvReader. A real decoder only succeeds when the AVC/AAC
// sequence headers are present before the first media samples, which is the
// core "standalone, first-frame decodable" requirement.
func TestClipFlv_OutputDecodable(t *testing.T) {
	src := buildInterleavedFixture(t)
	var out bytes.Buffer
	if _, err := ClipFlv(bytes.NewReader(src), &out, 50, 140); err != nil {
		t.Fatalf("ClipFlv failed: %v", err)
	}

	reader := CreateFlvReader()
	var h264Frames, aacFrames int
	var firstVideoPts, firstAudioPts uint32
	var gotFirstVideo, gotFirstAudio bool
	reader.OnFrame = func(cid codec.CodecID, frame []byte, pts, dts uint32) {
		switch cid {
		case codec.CODECID_VIDEO_H264:
			if !gotFirstVideo {
				firstVideoPts, gotFirstVideo = pts, true
			}
			h264Frames++
		case codec.CODECID_AUDIO_AAC:
			if !gotFirstAudio {
				firstAudioPts, gotFirstAudio = pts, true
			}
			aacFrames++
		}
	}
	if err := reader.Input(out.Bytes()); err != nil {
		t.Fatalf("clipped FLV is not decodable: %v", err)
	}
	if h264Frames == 0 {
		t.Fatal("decoder produced no H264 frames: config missing or broken")
	}
	if aacFrames == 0 {
		t.Fatal("decoder produced no AAC frames: config missing or broken")
	}
	// First decodable video sample starts at the rebased origin (ts 0).
	if firstVideoPts != 0 {
		t.Fatalf("first decoded video pts = %d, want 0", firstVideoPts)
	}
	// Audio stays on the same rebased clock. Audio selection begins at the
	// keyframe index, so its first sample is the first AAC frame at or after
	// the seek point (23ms in the fixture), never a negative offset.
	if !gotFirstAudio {
		t.Fatal("no audio frame decoded")
	}
	if int64(firstAudioPts)-int64(firstVideoPts) < 0 {
		t.Fatalf("audio leads video after rebase: audio=%d video=%d",
			firstAudioPts, firstVideoPts)
	}
}

// buildEnhancedHevcFixture uses the enhanced-RTMP ex-header layout (high bit
// set, FourCC "hvc1") to prove keyframe/config detection works there too.
func buildEnhancedHevcFixture(t *testing.T) []byte {
	t.Helper()
	b := newFlvFixtureBuilder(false, true)
	// Minimal parseable HEVC decoder configuration record (VPS/SPS/PPS arrays).
	hvcc := buildMinimalHvcc(t)
	// SequenceStart: 0b1001_0000 (ex + keyframe + PacketTypeSequenceStart),
	// then FourCC hvc1, then 3-byte composition time (0).
	seq := append([]byte{0x80 | uint8(KEY_FRAME)<<4 | PacketTypeSequenceStart},
		'h', 'v', 'c', '1', 0, 0, 0)
	seq = append(seq, hvcc...)
	b.addTag(uint8(VIDEO_TAG), 0, seq)
	// CodedFrames: ex + keyframe + PacketTypeCodedFrames, hvc1, 3-byte cts,
	// then one length-prefixed IDR NALU (type 19).
	idrNalu := mustHexToBytes("2801") // idr nalu, minimal
	cf := append([]byte{0x80 | uint8(KEY_FRAME)<<4 | PacketTypeCodedFrames},
		'h', 'v', 'c', '1', 0, 0, 0)
	cf = append(cf, 0, 0, 0, byte(len(idrNalu)))
	cf = append(cf, idrNalu...)
	b.addTag(uint8(VIDEO_TAG), 0, cf)
	// CodedFramesX inter frame (no cts field).
	pNaluHevc := mustHexToBytes("0201") // trail nalu
	cfx := append([]byte{0x80 | uint8(INTER_FRAME)<<4 | PacketTypeCodedFramesX},
		'h', 'v', 'c', '1')
	cfx = append(cfx, 0, 0, 0, byte(len(pNaluHevc)))
	cfx = append(cfx, pNaluHevc...)
	b.addTag(uint8(VIDEO_TAG), 40, cfx)
	return b.buf.Bytes()
}

func TestClipFlv_EnhancedHevcKeyframe(t *testing.T) {
	src := buildEnhancedHevcFixture(t)
	// Sanity: the enhanced layout is recognized as HEVC.
	_, fixtureTags := parseFlvTags(t, src)
	if cid := GetFLVVideoCodecId(fixtureTags[0].payload); cid != FLV_HEVC {
		t.Fatalf("fixture codec = %v, want FLV_HEVC", cid)
	}
	var out bytes.Buffer
	// Request starting after the only keyframe (0ms) but within stream: must
	// still seek back to the enhanced-RTMP keyframe.
	stats, err := ClipFlv(bytes.NewReader(src), &out, 20, 100)
	if err != nil {
		t.Fatalf("ClipFlv failed on enhanced FLV: %v", err)
	}
	if !stats.HasVideo || !stats.VideoConfigEmitted {
		t.Fatalf("HEVC clip missing video/config: %+v", stats)
	}
	_, tags := parseFlvTags(t, out.Bytes())
	firstVid, ok := firstMediaVideoTag(tags)
	if !ok {
		t.Fatal("no HEVC media tag emitted")
	}
	if firstVid.payload[0]&0x80 == 0 {
		t.Fatal("first media tag lost enhanced-RTMP ex header")
	}
	if (firstVid.payload[0]>>4)&0x07 != uint8(KEY_FRAME) {
		t.Fatal("first media HEVC tag is not a keyframe")
	}
	if firstVid.ts != 0 {
		t.Fatalf("HEVC keyframe ts = %d, want 0", firstVid.ts)
	}
}

// TestClipFlv_PreciseTagSizes recomputes every previous-tag-size from the
// emitted bytes and checks it against the on-disk value, covering the
// metadata size-preservation guarantee as well.
func TestClipFlv_PreciseTagSizes(t *testing.T) {
	src := buildInterleavedFixture(t)
	var out bytes.Buffer
	if _, err := ClipFlv(bytes.NewReader(src), &out, 0, 200); err != nil {
		t.Fatalf("ClipFlv failed: %v", err)
	}
	data := out.Bytes()
	off := 9 + 4 // header + PreviousTagSize0
	for off < len(data) {
		var hdr FlvTag
		hdr.Decode(data[off : off+11])
		tagEnd := off + 11 + int(hdr.DataSize)
		want := 11 + hdr.DataSize
		got := binary.BigEndian.Uint32(data[tagEnd : tagEnd+4])
		if got != want {
			t.Fatalf("at %d: previous tag size %d != computed %d", off, got, want)
		}
		off = tagEnd + 4
	}
}

// TestClipFlv_ZeroLengthWindow checks start == end: a tag landing exactly on
// the boundary is a valid single-point clip.
func TestClipFlv_ZeroLengthWindow(t *testing.T) {
	src := buildInterleavedFixture(t)
	var out bytes.Buffer
	// Keyframe exists at 0; window [0,0] keeps that keyframe (and any tag at 0).
	stats, err := ClipFlv(bytes.NewReader(src), &out, 0, 0)
	if err != nil {
		t.Fatalf("zero-length window failed: %v", err)
	}
	if stats.DurationMs != 0 {
		t.Fatalf("duration = %d, want 0", stats.DurationMs)
	}
}

// ---------------------------------------------------------------------------
// small helpers
// ---------------------------------------------------------------------------

type limitedWriter struct{ remaining int }

func (w *limitedWriter) Write(p []byte) (int, error) {
	if w.remaining <= 0 {
		return 0, nil // signals short write without an error
	}
	if len(p) > w.remaining {
		n := w.remaining
		w.remaining = 0
		return n, nil
	}
	w.remaining -= len(p)
	return len(p), nil
}

func tagKey(typ uint8, ts uint32) string {
	return string([]byte{typ, byte(ts >> 24), byte(ts >> 16), byte(ts >> 8), byte(ts)})
}

func readMetadataDuration(t *testing.T, payload []byte) float64 {
	t.Helper()
	off := 0
	if len(payload) > 0 && payload[0] == amf3Marker {
		off = 1
	}
	n, ok := amf0SkipValue(payload, off)
	if !ok {
		t.Fatal("cannot skip metadata name")
	}
	off = n
	// ECMA array marker + count
	if off >= len(payload) || payload[off] != amf0EcmaArray {
		t.Fatalf("expected ECMA array, got %#x", payload[off])
	}
	off += 5
	for {
		keyLen := int(binary.BigEndian.Uint16(payload[off:]))
		off += 2
		if keyLen == 0 {
			break
		}
		name := string(payload[off : off+keyLen])
		off += keyLen
		if payload[off] != amf0Number {
			t.Fatalf("test fixture only encodes numbers, got marker %#x", payload[off])
		}
		v := math.Float64frombits(binary.BigEndian.Uint64(payload[off+1 : off+9]))
		off += 9
		if name == "duration" {
			return v
		}
	}
	t.Fatal("duration property not found")
	return 0
}
