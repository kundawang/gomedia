package ogg

import (
	"bytes"
	"errors"

	"github.com/yapingcat/gomedia/go-codec"
)

type DemuxState int

const (
    DEMUX_PAGE_HEAD DemuxState = iota
    DEMUX_PAGE_PAYLOAD
)

type VideoParam struct {
    CodecId     codec.CodecID
    Width       uint32
    Height      uint32
    FrameRate   uint32
    Aspectratio uint32
    ExtraData   []byte
}

type AudioParam struct {
    CodecId        codec.CodecID
    SampleRate     uint32
    ChannelCount   uint32
    InitialPadding uint32
    ExtraData      []byte
}

type Demuxer struct {
    currentStream *oggStream
    streams       map[uint32]*oggStream
    // retired holds serial numbers whose segment has been finalized, so a
    // reuse in a later chain segment can be recognized instead of rejected.
    retired   map[uint32]bool
    headCache []byte
    OnPage    func(page *oggPage)
    OnPacket  func(streamId uint32, granule uint64, packet []byte, lost int)
    OnFrame   func(streamId uint32, cid codec.CodecID, frame []byte, pts uint64, dts uint64, lost int)
    // Chained Ogg callbacks. Left nil, the demuxer behaves exactly as the
    // single-segment version and only fires OnPage/OnPacket/OnFrame.
    OnSegmentBegin func(segment int, serial uint32)
    OnSegmentEnd   func(info *SegmentInfo)
    OnStreamFormat func(format *StreamFormat)
    OnChainFrame   func(frame *ChainFrame)
    state          DemuxState
    vparam         *VideoParam
    aparam         *AudioParam
    // chain state
    segment      *chainSegment
    segCount     int
    baseTimeMs   uint64
    lastAudioSR  uint32
    lastAudioCH  uint32
    lastVideoW   uint32
    lastVideoH   uint32
}

func NewDemuxer() *Demuxer {
    return &Demuxer{
        headCache: make([]byte, 0, 282),
        state:     DEMUX_PAGE_HEAD,
        streams:   make(map[uint32]*oggStream),
        retired:   make(map[uint32]bool),
    }
}

func (demuxer *Demuxer) Input(buf []byte) (err error) {

    if len(buf) == 0 {
        return nil
    }

    for {
        switch demuxer.state {
        case DEMUX_PAGE_HEAD:
            headLen := 0
            if len(demuxer.headCache)+len(buf) < 27 {
                demuxer.headCache = append(demuxer.headCache, buf...)
                return nil
            } else {
                segCount := 0
                if len(demuxer.headCache) >= 27 {
                    segCount = int(demuxer.headCache[26])
                } else {
                    segCount = int(buf[26-len(demuxer.headCache)])
                }

                if len(demuxer.headCache)+len(buf) < int(segCount)+27 {
                    demuxer.headCache = append(demuxer.headCache, buf...)
                    return nil
                }
                headLen = int(segCount) + 27
            }
            var hdr []byte
            if len(demuxer.headCache) > 0 {
                hdr = demuxer.headCache
                hdr = append(hdr, buf[:headLen-len(demuxer.headCache)]...)
            } else {
                hdr = buf
            }
            page, err := readPage(hdr)
            if err != nil {
                return err
            }
            if demuxer.OnPage != nil {
                demuxer.OnPage(page)
            }
            var stream *oggStream
            if page.isFirstPage {
                stream, err = demuxer.handleBOS(page)
                if err != nil {
                    return err
                }
            } else {
                var found bool
                stream, found = demuxer.streams[page.streamId]
                if !found {
                    // A data page without a preceding BOS: keep going with a
                    // synthetic stream instead of dropping the page.
                    if demuxer.segment == nil {
                        demuxer.beginSegment(page)
                    }
                    stream = demuxer.newStream(page)
                } else if stream.currentPage.pageSeq+1 != page.pageSeq {
                    stream.lost = 1
                    if demuxer.OnPacket != nil {
                        demuxer.OnPacket(stream.streamId, stream.currentPage.granulePos, stream.cache, 1)
                    }
                    err = demuxer.readPacket(stream, stream.cache)
                    if err != nil {
                        return err
                    }
                    stream.cache = stream.cache[:0]
                } else {
                    stream.lost = 0
                }
            }
            if page.eos {
                stream.hasEOS = true
                stream.eosGranule = page.granulePos
            }
            if demuxer.segment != nil && !page.isFirstPage {
                demuxer.segment.hasData = true
            }
            stream.currentPage = page
            demuxer.currentStream = stream
            demuxer.state = DEMUX_PAGE_PAYLOAD
            buf = buf[headLen-len(demuxer.headCache):]
            if len(demuxer.headCache) > 0 {
                demuxer.headCache = demuxer.headCache[:0]
            }
        case DEMUX_PAGE_PAYLOAD:
            stream := demuxer.currentStream
            page := stream.currentPage
            needLen := int(page.payloadLen) - len(page.cache)
            if needLen > len(buf) {
                page.cache = append(page.cache, buf...)
                return nil
            }

            var tmp []byte
            if len(page.cache) > 0 {
                page.cache = append(page.cache, buf[0:needLen]...)
                buf = buf[needLen:]
                tmp = page.cache
            } else {
                tmp = buf[0:page.payloadLen]
                buf = buf[page.payloadLen:]
            }

            idx := 0
            if stream.lost > 0 && page.isContinuePacket {
                removeLen := 0
                for ; idx < int(page.segmentsCount); idx++ {
                    if page.seqmentTable[idx] < 255 {
                        removeLen += int(page.seqmentTable[idx])
                    } else {
                        tmp = tmp[removeLen:]
                        break
                    }
                }
            } else if stream.lost == 0 && page.isContinuePacket {
                appendLen := 0
                for ; idx < int(page.segmentsCount); idx++ {
                    appendLen += int(page.seqmentTable[idx])
                    if page.seqmentTable[idx] < 255 {
                        stream.cache = append(stream.cache, tmp[:appendLen]...)
                        if demuxer.OnPacket != nil {
                            demuxer.OnPacket(stream.streamId, stream.currentPage.granulePos, stream.cache, 0)
                        }
                        page.packets = append(page.packets, stream.cache)
                        stream.cache = stream.cache[:0]
                        tmp = tmp[appendLen:]
                    }
                }
            }

            start := 0
            packetLen := 0
            for ; idx < int(page.segmentsCount); idx++ {
                packetLen += int(page.seqmentTable[idx])
                if page.seqmentTable[idx] < 255 {
                    packet := tmp[start : start+packetLen]
                    if demuxer.OnPacket != nil {
                        demuxer.OnPacket(stream.streamId, stream.currentPage.granulePos, packet, 0)
                    }
                    page.packets = append(page.packets, packet)
                    start = start + packetLen
                    packetLen = 0
                }
            }

            for _, pkt := range page.packets {
                if err := demuxer.readPacket(stream, pkt); err != nil {
                    return err
                }
            }

            if packetLen > 0 {
                stream.cache = append(stream.cache, tmp[start:]...)
            }
            page.cache = page.cache[:0]
            demuxer.state = DEMUX_PAGE_HEAD
            // All logical streams of this segment have reached EOS: close
            // the segment now, before the next chain segment's BOS arrives.
            if demuxer.segment != nil && stream.hasEOS {
                allEOS := true
                for _, s := range demuxer.streams {
                    if !s.hasEOS {
                        allEOS = false
                        break
                    }
                }
                if allEOS {
                    demuxer.endSegment(false)
                }
            }

        default:
            panic("unknow state")
        }
    }
}

func (demuxer *Demuxer) GetVideoParam() *VideoParam {
    return demuxer.vparam
}

func (demuxer *Demuxer) GetAudioParam() *AudioParam {
    return demuxer.aparam
}

func (demuxer *Demuxer) findCodec(stream *oggStream, packet []byte) {
    for _, ogg_codec := range codecs {
        if bytes.Equal(ogg_codec.magic(), packet[0:ogg_codec.magicSize()]) {
            stream.cid = ogg_codec.codecid()
            stream.parser = createParser(stream.cid)
            return
        }
    }
}

func (demuxer *Demuxer) readPacket(stream *oggStream, packet []byte) error {
    if stream.cid == codec.CODECID_UNRECOGNIZED {
        demuxer.findCodec(stream, packet)
    }

    if stream.cid == codec.CODECID_UNRECOGNIZED {
        return errors.New("not find codec id ")
    }

    switch stream.cid {
    case codec.CODECID_AUDIO_OPUS:
        // The first packets of every chain segment are the codec headers
        // (OpusHead then OpusTags), re-parsed from scratch so the new serial
        // and possibly new format never inherit the previous segment's state.
        if !stream.headersDone {
            if bytes.Equal([]byte("OpusHead"), packet[0:8]) ||
                bytes.Equal([]byte("OpusTags"), packet[0:8]) {
                if err := stream.parser.header(stream, packet); err != nil {
                    return err
                }
                if bytes.Equal([]byte("OpusHead"), packet[0:8]) {
                    opus, _ := stream.parser.(*opusDemuxer)
                    // Rebuilt for every segment: GetAudioParam always reports
                    // the format of the currently active segment.
                    demuxer.aparam = &AudioParam{
                        CodecId:        codec.CODECID_AUDIO_OPUS,
                        SampleRate:     uint32(opus.ctx.SampleRate),
                        ChannelCount:   uint32(opus.ctx.ChannelCount),
                        InitialPadding: uint32(opus.ctx.Preskip),
                        ExtraData:      opus.extradata,
                    }
                    demuxer.emitStreamFormat(stream)
                }
                return nil
            }
            // First non-header packet ends the header phase of the segment.
            stream.headersDone = true
        }
        frame, pts, dts := stream.parser.packet(stream, packet)
        if demuxer.OnFrame != nil {
            demuxer.OnFrame(stream.streamId, stream.cid, frame, pts, dts, stream.lost)
        }
        demuxer.emitChainFrame(stream, frame, stream.lost)
    case codec.CODECID_VIDEO_VP8:
        if !stream.headersDone {
            if bytes.Equal([]byte("OVP80"), packet[0:5]) {
                if err := stream.parser.header(stream, packet); err != nil {
                    return err
                }
                if packet[5] == 0x01 {
                    vp8, _ := stream.parser.(*vp8Demuxer)
                    demuxer.vparam = &VideoParam{
                        CodecId:     codec.CODECID_VIDEO_VP8,
                        Width:       uint32(vp8.width),
                        Height:      uint32(vp8.height),
                        FrameRate:   vp8.frameRate,
                        Aspectratio: vp8.sampleAspectratio,
                        ExtraData:   vp8.extradata,
                    }
                    demuxer.emitStreamFormat(stream)
                }
                return nil
            }
            stream.headersDone = true
        }
        frame, pts, dts := stream.parser.packet(stream, packet)
        if demuxer.OnFrame != nil {
            demuxer.OnFrame(stream.streamId, stream.cid, frame, pts, dts, stream.lost)
        }
        demuxer.emitChainFrame(stream, frame, stream.lost)
    default:
        return errors.New("unsupport  codec id ")
    }
    return nil
}
