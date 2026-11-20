package ogg

import (
    "bytes"
    "encoding/binary"
    "testing"

    "github.com/yapingcat/gomedia/go-codec"
)

const (
    htypeContinuation = 0x01
    htypeBOS          = 0x02
    htypeEOS          = 0x04
)

// buildPage assembles one ogg page with a valid checksum. When lastContinues
// is true the last packet is left open (its length must be a multiple of 255)
// so the page ends mid packet.
func buildPage(serial uint32, seq uint32, granule uint64, htype byte, packets [][]byte, lastContinues bool) []byte {
    var segTable []byte
    var payload []byte
    for i, p := range packets {
        payload = append(payload, p...)
        for j := 0; j < len(p)/255; j++ {
            segTable = append(segTable, 255)
        }
        if lastContinues && i == len(packets)-1 {
            if len(p)%255 != 0 {
                panic("continued fragment must be a multiple of 255")
            }
        } else {
            segTable = append(segTable, byte(len(p)%255))
        }
    }
    page := make([]byte, 0, 27+len(segTable)+len(payload))
    page = append(page, 'O', 'g', 'g', 'S', 0, htype)
    var tmp [8]byte
    binary.LittleEndian.PutUint64(tmp[:], granule)
    page = append(page, tmp[:]...)
    binary.LittleEndian.PutUint32(tmp[:4], serial)
    page = append(page, tmp[:4]...)
    binary.LittleEndian.PutUint32(tmp[:4], seq)
    page = append(page, tmp[:4]...)
    page = append(page, 0, 0, 0, 0) // crc placeholder
    page = append(page, byte(len(segTable)))
    page = append(page, segTable...)
    page = append(page, payload...)
    binary.LittleEndian.PutUint32(page[22:], computePageCRC(page))
    return page
}

func makeOpusHeadPacket() []byte {
    pkt := make([]byte, 19)
    copy(pkt, "OpusHead")
    pkt[8] = 1 // version
    pkt[9] = 2 // channels
    binary.LittleEndian.PutUint16(pkt[10:], 0)
    binary.LittleEndian.PutUint32(pkt[12:], 48000)
    binary.LittleEndian.PutUint16(pkt[16:], 0)
    pkt[18] = 0 // mapping family
    return pkt
}

func makeDataPacket(idx int, size int) []byte {
    pkt := make([]byte, size)
    pkt[0] = 0x00 // opus TOC: config 0, code 0
    pkt[1] = byte(idx)
    for i := 2; i < size; i++ {
        pkt[i] = byte(idx*31 + i)
    }
    return pkt
}

// buildControlStream returns an intact 4 page opus stream (serial 7) together
// with the pages and the packets it contains, in order.
func buildControlStream() (stream []byte, pages [][]byte, packets [][]byte) {
    head := makeOpusHeadPacket()
    d0 := makeDataPacket(0, 20)
    d1 := makeDataPacket(1, 20)
    d2 := makeDataPacket(2, 20)
    d3 := makeDataPacket(3, 20)
    d4 := makeDataPacket(4, 20)
    p0 := buildPage(7, 0, 0, htypeBOS, [][]byte{head}, false)
    p1 := buildPage(7, 1, 480, 0, [][]byte{d0, d1}, false)
    p2 := buildPage(7, 2, 960, 0, [][]byte{d2, d3}, false)
    p3 := buildPage(7, 3, 1440, htypeEOS, [][]byte{d4}, false)
    pages = [][]byte{p0, p1, p2, p3}
    for _, p := range pages {
        stream = append(stream, p...)
    }
    packets = [][]byte{head, d0, d1, d2, d3, d4}
    return stream, pages, packets
}

type collected struct {
    packets   [][]byte
    packetSid []uint32
    frameLost []int
    frameSid  []uint32
    errs      []*PageError
    gaps      []*GapEvent
}

func runDemux(mode CheckMode, stream []byte, chunk int, configure func(d *Demuxer)) (*collected, error) {
    c := &collected{}
    d := NewDemuxer()
    d.CheckMode = mode
    if configure != nil {
        configure(d)
    }
    d.OnPacket = func(streamId uint32, granule uint64, packet []byte, lost int) {
        pkt := make([]byte, len(packet))
        copy(pkt, packet)
        c.packets = append(c.packets, pkt)
        c.packetSid = append(c.packetSid, streamId)
    }
    d.OnFrame = func(streamId uint32, cid codec.CodecID, frame []byte, pts, dts uint64, lost int) {
        c.frameLost = append(c.frameLost, lost)
        c.frameSid = append(c.frameSid, streamId)
    }
    d.OnPageError = func(perr *PageError) { c.errs = append(c.errs, perr) }
    d.OnGap = func(gap *GapEvent) { c.gaps = append(c.gaps, gap) }
    var err error
    for i := 0; i < len(stream); i += chunk {
        end := i + chunk
        if end > len(stream) {
            end = len(stream)
        }
        if e := d.Input(stream[i:end]); e != nil {
            err = e
            break
        }
    }
    if err == nil {
        err = d.Flush()
    }
    return c, err
}

func assertPackets(t *testing.T, c *collected, want [][]byte) {
    t.Helper()
    if len(c.packets) != len(want) {
        t.Fatalf("packet count: got %d, want %d", len(c.packets), len(want))
    }
    for i := range want {
        if !bytes.Equal(c.packets[i], want[i]) {
            t.Fatalf("packet %d: got %d bytes %v..., want %d bytes %v...", i,
                len(c.packets[i]), c.packets[i][:min(8, len(c.packets[i]))],
                len(want[i]), want[i][:min(8, len(want[i]))])
        }
    }
}

func min(a, b int) int {
    if a < b {
        return a
    }
    return b
}

// Intact control stream: no diagnostics in any mode, all packets in order.
func TestCheckIntactControlStream(t *testing.T) {
    stream, _, packets := buildControlStream()
    for _, mode := range []CheckMode{CHECK_NONE, CHECK_STRICT, CHECK_RECOVER} {
        for _, chunk := range []int{4096, 7} {
            c, err := runDemux(mode, stream, chunk, nil)
            if err != nil {
                t.Fatalf("mode %d chunk %d: unexpected error %v", mode, chunk, err)
            }
            if len(c.errs) != 0 {
                t.Fatalf("mode %d chunk %d: unexpected page errors %v", mode, chunk, c.errs)
            }
            if len(c.gaps) != 0 {
                t.Fatalf("mode %d chunk %d: unexpected gaps %v", mode, chunk, c.gaps)
            }
            assertPackets(t, c, packets)
            for i, lost := range c.frameLost {
                if lost != 0 {
                    t.Fatalf("mode %d chunk %d: frame %d unexpectedly lost", mode, chunk, i)
                }
            }
        }
    }
}

// Single byte flip inside page 2 payload: CRC diagnostic must point at the
// page offset, recovery must drop page 2 and continue with page 3.
func TestCheckCRCErrorRecovery(t *testing.T) {
    stream, pages, packets := buildControlStream()
    p2offset := len(pages[0]) + len(pages[1])
    stream[p2offset+27+2+5] ^= 0xFF // flip one payload byte of page 2

    c, err := runDemux(CHECK_RECOVER, stream, 4096, nil)
    if err != nil {
        t.Fatalf("recover mode must not fail, got %v", err)
    }
    if len(c.errs) != 2 {
        t.Fatalf("expected 2 page errors (crc + sequence), got %v", c.errs)
    }
    crcErr := c.errs[0]
    if crcErr.Reason != ERR_REASON_CRC {
        t.Fatalf("expected crc error, got %v", crcErr)
    }
    if crcErr.Offset != int64(p2offset) || crcErr.PageSeq != 2 || crcErr.StreamID != 7 {
        t.Fatalf("bad crc diagnostic: offset %d (want %d), seq %d, serial %d",
            crcErr.Offset, p2offset, crcErr.PageSeq, crcErr.StreamID)
    }
    seqErr := c.errs[1]
    if seqErr.Reason != ERR_REASON_SEQUENCE || seqErr.PageSeq != 3 {
        t.Fatalf("expected sequence error for page 3, got %v", seqErr)
    }
    if len(c.gaps) == 0 || c.gaps[0].DroppedBytes != len(pages[2]) || c.gaps[0].Offset != int64(p2offset) {
        t.Fatalf("expected gap dropping %d bytes at %d, got %v", len(pages[2]), p2offset, c.gaps)
    }
    // page 2 packets are gone, page 3 packet recovered with lost flag
    assertPackets(t, c, [][]byte{packets[0], packets[1], packets[2], packets[5]})
    if len(c.frameLost) != 3 || c.frameLost[0] != 0 || c.frameLost[1] != 0 || c.frameLost[2] != 1 {
        t.Fatalf("unexpected frame lost flags %v", c.frameLost)
    }
}

// Strict mode stops at the first error.
func TestCheckCRCErrorStrict(t *testing.T) {
    stream, pages, packets := buildControlStream()
    p2offset := len(pages[0]) + len(pages[1])
    stream[p2offset+27+2+5] ^= 0xFF

    c, err := runDemux(CHECK_STRICT, stream, 4096, nil)
    if err == nil {
        t.Fatal("strict mode must return an error")
    }
    perr, ok := err.(*PageError)
    if !ok || perr.Reason != ERR_REASON_CRC || perr.Offset != int64(p2offset) || perr.PageSeq != 2 {
        t.Fatalf("unexpected strict error %v", err)
    }
    // pages after the damaged one must not be delivered
    assertPackets(t, c, packets[:3])
}

// A missing page shows up as a sequence gap; later pages still demux.
func TestCheckMissingPage(t *testing.T) {
    _, pages, packets := buildControlStream()
    stream := append(append(append([]byte{}, pages[0]...), pages[2]...), pages[3]...)

    c, err := runDemux(CHECK_RECOVER, stream, 4096, nil)
    if err != nil {
        t.Fatalf("recover mode must not fail, got %v", err)
    }
    if len(c.errs) != 1 {
        t.Fatalf("expected 1 page error, got %v", c.errs)
    }
    seqErr := c.errs[0]
    if seqErr.Reason != ERR_REASON_SEQUENCE || seqErr.Offset != int64(len(pages[0])) || seqErr.PageSeq != 2 {
        t.Fatalf("bad sequence diagnostic %v", seqErr)
    }
    if len(c.gaps) != 1 || c.gaps[0].LostPages != 1 || c.gaps[0].StreamID != 7 {
        t.Fatalf("expected gap with 1 lost page, got %v", c.gaps)
    }
    assertPackets(t, c, [][]byte{packets[0], packets[3], packets[4], packets[5]})
    if len(c.frameLost) != 3 || c.frameLost[0] != 1 || c.frameLost[1] != 1 || c.frameLost[2] != 0 {
        t.Fatalf("unexpected frame lost flags %v", c.frameLost)
    }
}

// Garbage plus a fake "OggS" capture header between two pages: both are
// reported and skipped, the real pages demux without any loss flag.
func TestCheckFakeCaptureHeader(t *testing.T) {
    _, pages, packets := buildControlStream()
    garbage := bytes.Repeat([]byte{0xAB}, 10)
    fake := []byte{'O', 'g', 'g', 'S', 0, 0}
    fake = append(fake, make([]byte, 8)...)      // granule
    fake = append(fake, 9, 0, 0, 0)              // serial
    fake = append(fake, 0, 0, 0, 0)              // seq
    fake = append(fake, 0xEF, 0xBE, 0xAD, 0xDE)  // bad crc
    fake = append(fake, 1, 20)                   // 1 segment of 20 bytes
    fake = append(fake, make([]byte, 20)...)     // payload

    var stream []byte
    stream = append(stream, pages[0]...)
    stream = append(stream, pages[1]...)
    garbageOffset := len(stream)
    stream = append(stream, garbage...)
    fakeOffset := len(stream)
    stream = append(stream, fake...)
    stream = append(stream, pages[2]...)
    stream = append(stream, pages[3]...)

    c, err := runDemux(CHECK_RECOVER, stream, 4096, nil)
    if err != nil {
        t.Fatalf("recover mode must not fail, got %v", err)
    }
    if len(c.errs) != 2 {
        t.Fatalf("expected 2 page errors (capture + crc), got %v", c.errs)
    }
    if c.errs[0].Reason != ERR_REASON_CAPTURE || c.errs[0].Offset != int64(garbageOffset) {
        t.Fatalf("bad capture diagnostic %v", c.errs[0])
    }
    if c.errs[1].Reason != ERR_REASON_CRC || c.errs[1].Offset != int64(fakeOffset) {
        t.Fatalf("bad crc diagnostic %v", c.errs[1])
    }
    dropped := 0
    for _, g := range c.gaps {
        dropped += g.DroppedBytes
    }
    if dropped != len(garbage)+len(fake) {
        t.Fatalf("expected %d dropped bytes, got %d (%v)", len(garbage)+len(fake), dropped, c.gaps)
    }
    assertPackets(t, c, packets)
    for i, lost := range c.frameLost {
        if lost != 0 {
            t.Fatalf("frame %d unexpectedly lost", i)
        }
    }
}

// Interleaved serials: corruption on serial 7 must not affect serial 8.
func TestCheckInterleavedSerials(t *testing.T) {
    head := makeOpusHeadPacket()
    ad0 := makeDataPacket(10, 20)
    ad1 := makeDataPacket(11, 20)
    ad2 := makeDataPacket(12, 20)
    bd0 := makeDataPacket(20, 20)
    bd1 := makeDataPacket(21, 20)
    bd2 := makeDataPacket(22, 20)

    a0 := buildPage(7, 0, 0, htypeBOS, [][]byte{head}, false)
    b0 := buildPage(8, 0, 0, htypeBOS, [][]byte{head}, false)
    a1 := buildPage(7, 1, 480, 0, [][]byte{ad0}, false)
    b1 := buildPage(8, 1, 480, 0, [][]byte{bd0}, false)
    a2 := buildPage(7, 2, 960, 0, [][]byte{ad1}, false)
    b2 := buildPage(8, 2, 960, 0, [][]byte{bd1}, false)
    a3 := buildPage(7, 3, 1440, 0, [][]byte{ad2}, false)
    b3 := buildPage(8, 3, 1440, htypeEOS, [][]byte{bd2}, false)

    var stream []byte
    for _, p := range [][]byte{a0, b0, a1, b1, a2, b2, a3, b3} {
        stream = append(stream, p...)
    }
    a2offset := len(a0) + len(b0) + len(a1) + len(b1)
    stream[a2offset+28] ^= 0xFF // corrupt a2 payload

    c, err := runDemux(CHECK_RECOVER, stream, 4096, nil)
    if err != nil {
        t.Fatalf("recover mode must not fail, got %v", err)
    }
    if len(c.errs) != 2 || c.errs[0].Reason != ERR_REASON_CRC || c.errs[1].Reason != ERR_REASON_SEQUENCE {
        t.Fatalf("unexpected errors %v", c.errs)
    }
    if c.errs[0].Offset != int64(a2offset) || c.errs[0].StreamID != 7 || c.errs[0].PageSeq != 2 {
        t.Fatalf("bad crc diagnostic %v", c.errs[0])
    }
    want := [][]byte{head, head, ad0, bd0, bd1, ad2, bd2}
    wantSid := []uint32{7, 8, 7, 8, 8, 7, 8}
    if len(c.packets) != len(want) {
        t.Fatalf("packet count: got %d, want %d", len(c.packets), len(want))
    }
    for i := range want {
        if !bytes.Equal(c.packets[i], want[i]) || c.packetSid[i] != wantSid[i] {
            t.Fatalf("packet %d: sid %d content mismatch", i, c.packetSid[i])
        }
    }
    // only the serial 7 frame after the gap is flagged lost
    for i := range c.frameLost {
        want := 0
        if c.frameSid[i] == 7 && i > 0 {
            want = 1
        }
        if c.frameLost[i] != want {
            t.Fatalf("frame %d (sid %d): lost=%d want %d", i, c.frameSid[i], c.frameLost[i], want)
        }
    }
}

// Truncated tail: reported at Flush time.
func TestCheckEOFTruncated(t *testing.T) {
    stream, pages, packets := buildControlStream()
    truncated := stream[:len(stream)-15]
    p3offset := len(stream) - len(pages[3])

    c, err := runDemux(CHECK_RECOVER, truncated, 4096, nil)
    if err != nil {
        t.Fatalf("recover mode Flush must not fail, got %v", err)
    }
    if len(c.errs) != 1 || c.errs[0].Reason != ERR_REASON_EOF_TRUNCATED {
        t.Fatalf("expected truncation error, got %v", c.errs)
    }
    if c.errs[0].Offset != int64(p3offset) || c.errs[0].PageSeq != 3 {
        t.Fatalf("bad truncation diagnostic %v", c.errs[0])
    }
    assertPackets(t, c, packets[:5])

    _, err = runDemux(CHECK_STRICT, truncated, 4096, nil)
    perr, ok := err.(*PageError)
    if !ok || perr.Reason != ERR_REASON_EOF_TRUNCATED {
        t.Fatalf("strict mode must return the truncation error, got %v", err)
    }
}

// Resync scan limit: garbage longer than MaxScanBytes without a capture
// pattern is fatal, further input is rejected.
func TestCheckScanLimit(t *testing.T) {
    _, pages, _ := buildControlStream()
    var stream []byte
    stream = append(stream, pages[0]...)
    stream = append(stream, pages[1]...)
    stream = append(stream, bytes.Repeat([]byte{0x00}, 200)...)
    stream = append(stream, pages[2]...)
    stream = append(stream, pages[3]...)

    c, err := runDemux(CHECK_RECOVER, stream, 4096, func(d *Demuxer) { d.MaxScanBytes = 64 })
    if err == nil {
        t.Fatal("expected scan limit error")
    }
    perr, ok := err.(*PageError)
    if !ok || perr.Reason != ERR_REASON_SCAN_LIMIT {
        t.Fatalf("expected scan limit error, got %v", err)
    }
    found := false
    for _, e := range c.errs {
        if e.Reason == ERR_REASON_SCAN_LIMIT {
            found = true
        }
    }
    if !found {
        t.Fatalf("scan limit error not reported through OnPageError: %v", c.errs)
    }
}

// A page larger than MaxPageBytes is treated as a false capture header.
func TestCheckOversizedPage(t *testing.T) {
    head := makeOpusHeadPacket()
    d0 := makeDataPacket(0, 20)
    d1 := makeDataPacket(1, 20)
    big := makeDataPacket(2, 200)
    d4 := makeDataPacket(4, 20)
    p0 := buildPage(7, 0, 0, htypeBOS, [][]byte{head}, false)
    p1 := buildPage(7, 1, 480, 0, [][]byte{d0, d1}, false)
    p2 := buildPage(7, 2, 960, 0, [][]byte{big}, false)
    p3 := buildPage(7, 3, 1440, htypeEOS, [][]byte{d4}, false)

    var stream []byte
    for _, p := range [][]byte{p0, p1, p2, p3} {
        stream = append(stream, p...)
    }
    p2offset := len(p0) + len(p1)

    c, err := runDemux(CHECK_RECOVER, stream, 4096, func(d *Demuxer) { d.MaxPageBytes = 100 })
    if err != nil {
        t.Fatalf("recover mode must not fail, got %v", err)
    }
    if len(c.errs) < 1 || c.errs[0].Reason != ERR_REASON_PAGE_TOO_LONG {
        t.Fatalf("expected page-too-long error, got %v", c.errs)
    }
    if c.errs[0].Offset != int64(p2offset) || c.errs[0].PageSeq != 2 {
        t.Fatalf("bad page-too-long diagnostic %v", c.errs[0])
    }
    assertPackets(t, c, [][]byte{head, d0, d1, d4})
}

// Legitimate 0xffffffff -> 0 sequence wraparound is accepted, a backwards
// jump is reported.
func TestCheckSequenceWraparound(t *testing.T) {
    head := makeOpusHeadPacket()
    w0 := buildPage(9, 0xFFFFFFFE, 0, htypeBOS, [][]byte{head}, false)
    w1 := buildPage(9, 0xFFFFFFFF, 480, 0, [][]byte{makeDataPacket(0, 20)}, false)
    w2 := buildPage(9, 0, 960, 0, [][]byte{makeDataPacket(1, 20)}, false)
    w3 := buildPage(9, 1, 1440, 0, [][]byte{makeDataPacket(2, 20)}, false)
    w4 := buildPage(9, 0, 1920, 0, [][]byte{makeDataPacket(3, 20)}, false) // backwards jump

    var stream []byte
    for _, p := range [][]byte{w0, w1, w2, w3, w4} {
        stream = append(stream, p...)
    }
    w4offset := len(w0) + len(w1) + len(w2) + len(w3)

    c, err := runDemux(CHECK_RECOVER, stream, 4096, nil)
    if err != nil {
        t.Fatalf("recover mode must not fail, got %v", err)
    }
    if len(c.errs) != 1 || c.errs[0].Reason != ERR_REASON_SEQUENCE {
        t.Fatalf("expected exactly one sequence error for the backwards jump, got %v", c.errs)
    }
    if c.errs[0].Offset != int64(w4offset) || c.errs[0].PageSeq != 0 {
        t.Fatalf("bad sequence diagnostic %v", c.errs[0])
    }
    if len(c.packets) != 5 {
        t.Fatalf("expected all 5 packets, got %d", len(c.packets))
    }
}

// Continuation flags: a legitimately continued packet is joined, a page with
// a false continuation flag is reported and dropped.
func TestCheckContinuationFlag(t *testing.T) {
    head := makeOpusHeadPacket()
    d0 := makeDataPacket(0, 20)
    frag := makeDataPacket(90, 255) // continued onto the next page
    rest := makeDataPacket(91, 50)
    dx := makeDataPacket(92, 20)
    d4 := makeDataPacket(4, 20)

    c0 := buildPage(7, 0, 0, htypeBOS, [][]byte{head}, false)
    c1 := buildPage(7, 1, 480, 0, [][]byte{d0, frag}, true) // ends mid packet
    c2 := buildPage(7, 2, 960, htypeContinuation, [][]byte{rest}, false)
    c3 := buildPage(7, 3, 1440, htypeContinuation, [][]byte{dx}, false) // illegal: c2 ended closed
    c4 := buildPage(7, 4, 1920, htypeEOS, [][]byte{d4}, false)

    var stream []byte
    for _, p := range [][]byte{c0, c1, c2, c3, c4} {
        stream = append(stream, p...)
    }
    c3offset := len(c0) + len(c1) + len(c2)

    c, err := runDemux(CHECK_RECOVER, stream, 4096, nil)
    if err != nil {
        t.Fatalf("recover mode must not fail, got %v", err)
    }
    if len(c.errs) != 1 || c.errs[0].Reason != ERR_REASON_CONTINUATION {
        t.Fatalf("expected exactly one continuation error, got %v", c.errs)
    }
    if c.errs[0].Offset != int64(c3offset) || c.errs[0].PageSeq != 3 {
        t.Fatalf("bad continuation diagnostic %v", c.errs[0])
    }
    joined := append(append([]byte{}, frag...), rest...)
    assertPackets(t, c, [][]byte{head, d0, joined, d4})
    if len(c.frameLost) != 3 || c.frameLost[0] != 0 || c.frameLost[1] != 0 || c.frameLost[2] != 1 {
        t.Fatalf("unexpected frame lost flags %v", c.frameLost)
    }
}
