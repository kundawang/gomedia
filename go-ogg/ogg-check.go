package ogg

import (
    "bytes"
    "encoding/binary"
    "fmt"
)

// CheckMode selects how the demuxer verifies page integrity.
type CheckMode int

const (
    // CHECK_NONE keeps the historical demux behavior: pages are parsed
    // without CRC / sequence / continuation verification. This is the default.
    CHECK_NONE CheckMode = iota
    // CHECK_STRICT verifies every page and stops at the first error:
    // Input/Flush report it through OnPageError and return it.
    CHECK_STRICT
    // CHECK_RECOVER verifies every page, reports errors through OnPageError,
    // drops unrecoverable data and scans forward (up to MaxScanBytes) for the
    // next intact page, emitting GapEvent-s for everything that was lost.
    CHECK_RECOVER
)

// ErrorReason identifies why a page (or a byte range) was rejected.
type ErrorReason int

const (
    // ERR_REASON_CRC: the page checksum does not match its content.
    ERR_REASON_CRC ErrorReason = iota
    // ERR_REASON_CAPTURE: no "OggS" capture pattern at the page boundary.
    ERR_REASON_CAPTURE
    // ERR_REASON_VERSION: unsupported stream structure version.
    ERR_REASON_VERSION
    // ERR_REASON_SEQUENCE: page sequence gap or backwards jump within one serial.
    ERR_REASON_SEQUENCE
    // ERR_REASON_CONTINUATION: continuation flag does not match the previous page.
    ERR_REASON_CONTINUATION
    // ERR_REASON_PAGE_TOO_LONG: page exceeds MaxPageBytes (fake capture header).
    ERR_REASON_PAGE_TOO_LONG
    // ERR_REASON_SCAN_LIMIT: no capture pattern found within MaxScanBytes.
    ERR_REASON_SCAN_LIMIT
    // ERR_REASON_EOF_TRUNCATED: the stream ends in the middle of a page.
    ERR_REASON_EOF_TRUNCATED
)

func (reason ErrorReason) String() string {
    switch reason {
    case ERR_REASON_CRC:
        return "crc mismatch"
    case ERR_REASON_CAPTURE:
        return "capture pattern not found"
    case ERR_REASON_VERSION:
        return "unsupported version"
    case ERR_REASON_SEQUENCE:
        return "page sequence error"
    case ERR_REASON_CONTINUATION:
        return "continuation flag mismatch"
    case ERR_REASON_PAGE_TOO_LONG:
        return "page too long"
    case ERR_REASON_SCAN_LIMIT:
        return "resync scan limit exceeded"
    case ERR_REASON_EOF_TRUNCATED:
        return "truncated at end of stream"
    default:
        return "unknown error"
    }
}

// PageError describes one integrity problem found while checking a stream.
type PageError struct {
    Offset   int64  // absolute byte offset of the bad page / damaged region in the stream
    StreamID uint32 // serial of the offending page (0 when unknown)
    PageSeq  uint32 // sequence number of the offending page (0 when unknown)
    Reason   ErrorReason
    Detail   string
}

func (e *PageError) Error() string {
    return fmt.Sprintf("ogg integrity error at byte offset %d (serial %d, page seq %d): %s: %s",
        e.Offset, e.StreamID, e.PageSeq, e.Reason, e.Detail)
}

// GapEvent describes a hole in the stream that recovery skipped over:
// dropped bytes while resyncing, a dropped page, or lost pages detected
// through a sequence jump.
type GapEvent struct {
    Offset       int64  // absolute byte offset where the gap starts
    DroppedBytes int    // bytes dropped while resyncing (0 for pure sequence gaps)
    StreamID     uint32 // affected serial (0 when unknown)
    LostPages    int    // number of pages lost (sequence gaps only, 0 otherwise)
    Reason       ErrorReason
}

const (
    // DEFAULT_MAX_SCAN_BYTES is the default resync scan limit for CHECK_RECOVER.
    DEFAULT_MAX_SCAN_BYTES = 256 * 1024
    // MAX_OGG_PAGE_BYTES is the largest possible ogg page:
    // 27 byte header + 255 segment table entries + 255*255 payload bytes.
    MAX_OGG_PAGE_BYTES = 27 + 255 + 255*255
)

// serialCheckState tracks the per-serial expectations used to validate the
// next page of that serial (interleaved serials are tracked independently).
type serialCheckState struct {
    hasPrev        bool
    lastSeq        uint32
    expectContinue bool // previous page of this serial ended mid packet
}

func (demuxer *Demuxer) maxScanBytes() int {
    if demuxer.MaxScanBytes > 0 {
        return demuxer.MaxScanBytes
    }
    return DEFAULT_MAX_SCAN_BYTES
}

func (demuxer *Demuxer) maxPageBytes() int {
    if demuxer.MaxPageBytes > 0 {
        return demuxer.MaxPageBytes
    }
    return MAX_OGG_PAGE_BYTES
}

func (demuxer *Demuxer) emitPageError(perr *PageError) {
    if demuxer.OnPageError != nil {
        demuxer.OnPageError(perr)
    }
}

func (demuxer *Demuxer) emitGap(gap *GapEvent) {
    if demuxer.OnGap != nil {
        demuxer.OnGap(gap)
    }
}

func (demuxer *Demuxer) dropCheckedBytes(n int) {
    demuxer.checkBuf = demuxer.checkBuf[n:]
    demuxer.checkOffset += int64(n)
    if len(demuxer.checkBuf) == 0 {
        demuxer.checkBuf = nil
    }
}

// computePageCRC calculates the ogg CRC of a full page, treating the
// checksum field itself (bytes 22..25) as zero.
func computePageCRC(page []byte) uint32 {
    var crc uint32
    for i := 0; i < len(page); i++ {
        b := page[i]
        if i >= 22 && i < 26 {
            b = 0
        }
        crc = (crc << 8) ^ crc_table[byte(crc>>24)^b]
    }
    return crc
}

func pageEndsOpen(page *oggPage) bool {
    return page.segmentsCount > 0 && page.seqmentTable[page.segmentsCount-1] == 255
}

func (demuxer *Demuxer) inputChecked(buf []byte) error {
    if demuxer.halted != nil {
        return demuxer.halted
    }
    if demuxer.checkStates == nil {
        demuxer.checkStates = make(map[uint32]*serialCheckState)
    }
    demuxer.checkBuf = append(demuxer.checkBuf, buf...)
    for {
        progress, err := demuxer.checkStep(false)
        if err != nil {
            demuxer.halted = err
            return err
        }
        if !progress {
            return nil
        }
    }
}

// checkStep consumes at most one page (or performs one resync) from checkBuf.
// progress=true means bytes were consumed and another step may succeed.
// A non-nil error is always fatal for the demuxer: in CHECK_STRICT mode every
// integrity error is fatal, in CHECK_RECOVER mode only an exhausted resync
// scan limit (or a demux failure on a valid page) is fatal. Recoverable
// errors are reported through OnPageError inside the step.
func (demuxer *Demuxer) checkStep(finish bool) (progress bool, err error) {
    buf := demuxer.checkBuf
    offset := demuxer.checkOffset

    if len(buf) == 0 {
        return false, nil
    }

    // capture pattern at the page boundary
    if len(buf) >= len(CapturePattern) && !bytes.Equal(buf[:4], CapturePattern[:]) {
        perr := &PageError{Offset: offset, Reason: ERR_REASON_CAPTURE,
            Detail: "capture pattern not found at page boundary"}
        demuxer.emitPageError(perr)
        if demuxer.CheckMode == CHECK_STRICT {
            return false, perr
        }
        return demuxer.resync(ERR_REASON_CAPTURE, finish)
    }

    // fixed 27 byte header
    if len(buf) < 27 {
        if finish {
            perr := &PageError{Offset: offset, Reason: ERR_REASON_EOF_TRUNCATED,
                Detail: fmt.Sprintf("stream ends with %d truncated bytes", len(buf))}
            demuxer.emitPageError(perr)
            demuxer.dropCheckedBytes(len(buf))
            if demuxer.CheckMode == CHECK_STRICT {
                return false, perr
            }
        }
        return false, nil
    }

    segCount := int(buf[26])
    headLen := 27 + segCount
    if len(buf) < headLen {
        if finish {
            perr := &PageError{Offset: offset, Reason: ERR_REASON_EOF_TRUNCATED,
                Detail: "stream ends inside the page segment table"}
            demuxer.emitPageError(perr)
            demuxer.dropCheckedBytes(len(buf))
            if demuxer.CheckMode == CHECK_STRICT {
                return false, perr
            }
        }
        return false, nil
    }

    payloadLen := 0
    for i := 0; i < segCount; i++ {
        payloadLen += int(buf[27+i])
    }
    pageLen := headLen + payloadLen

    if pageLen > demuxer.maxPageBytes() {
        perr := &PageError{Offset: offset, Reason: ERR_REASON_PAGE_TOO_LONG,
            StreamID: binary.LittleEndian.Uint32(buf[14:18]),
            PageSeq:  binary.LittleEndian.Uint32(buf[18:22]),
            Detail:   fmt.Sprintf("page size %d exceeds limit %d, treating capture pattern as false positive", pageLen, demuxer.maxPageBytes())}
        demuxer.emitPageError(perr)
        if demuxer.CheckMode == CHECK_STRICT {
            return false, perr
        }
        return demuxer.resync(ERR_REASON_PAGE_TOO_LONG, finish)
    }

    if len(buf) < pageLen {
        if finish {
            perr := &PageError{Offset: offset, Reason: ERR_REASON_EOF_TRUNCATED,
                StreamID: binary.LittleEndian.Uint32(buf[14:18]),
                PageSeq:  binary.LittleEndian.Uint32(buf[18:22]),
                Detail:   fmt.Sprintf("stream ends inside page: have %d of %d bytes", len(buf), pageLen)}
            demuxer.emitPageError(perr)
            demuxer.dropCheckedBytes(len(buf))
            if demuxer.CheckMode == CHECK_STRICT {
                return false, perr
            }
        }
        return false, nil
    }

    if buf[4] != 0 {
        perr := &PageError{Offset: offset, Reason: ERR_REASON_VERSION,
            StreamID: binary.LittleEndian.Uint32(buf[14:18]),
            PageSeq:  binary.LittleEndian.Uint32(buf[18:22]),
            Detail:   fmt.Sprintf("unsupported stream structure version %d", buf[4])}
        demuxer.emitPageError(perr)
        if demuxer.CheckMode == CHECK_STRICT {
            return false, perr
        }
        return demuxer.resync(ERR_REASON_VERSION, finish)
    }

    storedCRC := binary.LittleEndian.Uint32(buf[22:26])
    if computed := computePageCRC(buf[:pageLen]); storedCRC != computed {
        perr := &PageError{Offset: offset, Reason: ERR_REASON_CRC,
            StreamID: binary.LittleEndian.Uint32(buf[14:18]),
            PageSeq:  binary.LittleEndian.Uint32(buf[18:22]),
            Detail:   fmt.Sprintf("stored crc 0x%08x, computed 0x%08x", storedCRC, computed)}
        demuxer.emitPageError(perr)
        if demuxer.CheckMode == CHECK_STRICT {
            return false, perr
        }
        return demuxer.resync(ERR_REASON_CRC, finish)
    }

    page, err := readPage(buf[:headLen])
    if err != nil {
        return false, err
    }

    st := demuxer.checkStates[page.streamId]
    if st == nil {
        st = &serialCheckState{}
        demuxer.checkStates[page.streamId] = st
    }

    // page sequence within this serial (uint32 arithmetic accepts the
    // legitimate 0xffffffff -> 0 wraparound, catches gaps and backwards jumps)
    if st.hasPrev {
        expected := st.lastSeq + 1
        if delta := page.pageSeq - expected; delta != 0 {
            var detail string
            lost := 0
            if int32(delta) > 0 {
                lost = int(delta)
                detail = fmt.Sprintf("page sequence gap: expected %d, got %d (%d page(s) lost)", expected, page.pageSeq, lost)
            } else {
                detail = fmt.Sprintf("page sequence moved backwards: expected %d, got %d (wraparound or duplicated pages)", expected, page.pageSeq)
            }
            perr := &PageError{Offset: offset, StreamID: page.streamId, PageSeq: page.pageSeq,
                Reason: ERR_REASON_SEQUENCE, Detail: detail}
            demuxer.emitPageError(perr)
            if demuxer.CheckMode == CHECK_STRICT {
                return false, perr
            }
            demuxer.emitGap(&GapEvent{Offset: offset, StreamID: page.streamId, LostPages: lost, Reason: ERR_REASON_SEQUENCE})
        }
    }

    // continuation flag must match the way the previous page of this serial ended
    if (st.hasPrev && page.isContinuePacket != st.expectContinue) || (!st.hasPrev && page.isContinuePacket) {
        var detail string
        switch {
        case !st.hasPrev:
            detail = "first page of serial claims a continued packet"
        case page.isContinuePacket:
            detail = "page claims a continued packet but the previous page ended on a packet boundary"
        default:
            detail = "previous page ended mid packet but this page does not continue it"
        }
        perr := &PageError{Offset: offset, StreamID: page.streamId, PageSeq: page.pageSeq,
            Reason: ERR_REASON_CONTINUATION, Detail: detail}
        demuxer.emitPageError(perr)
        if demuxer.CheckMode == CHECK_STRICT {
            return false, perr
        }
        // drop the unjoinable partial packet the demuxer may have cached
        if stream, found := demuxer.streams[page.streamId]; found {
            stream.cache = stream.cache[:0]
            stream.lost = 1
        }
        if page.isContinuePacket {
            // the page starts with a fragment whose head is lost: the whole
            // page cannot be joined reliably, drop it and continue with the
            // next page
            demuxer.emitGap(&GapEvent{Offset: offset, DroppedBytes: pageLen, StreamID: page.streamId, Reason: ERR_REASON_CONTINUATION})
            st.hasPrev = true
            st.lastSeq = page.pageSeq
            st.expectContinue = pageEndsOpen(page)
            demuxer.dropCheckedBytes(pageLen)
            return true, nil
        }
    }

    st.hasPrev = true
    st.lastSeq = page.pageSeq
    st.expectContinue = pageEndsOpen(page)

    if err := demuxer.inputBytes(buf[:pageLen]); err != nil {
        return false, err
    }
    demuxer.dropCheckedBytes(pageLen)
    return true, nil
}

// resync scans forward (from the byte after the current position) for the
// next capture pattern and drops everything before it as a gap. It is used in
// CHECK_RECOVER mode after a page failed verification.
func (demuxer *Demuxer) resync(reason ErrorReason, finish bool) (bool, error) {
    limit := demuxer.maxScanBytes()
    window := demuxer.checkBuf[1:]
    limited := false
    if len(window) > limit {
        window = window[:limit]
        limited = true
    }
    idx := bytes.Index(window, CapturePattern[:])
    if idx < 0 {
        if limited {
            perr := &PageError{Offset: demuxer.checkOffset, Reason: ERR_REASON_SCAN_LIMIT,
                Detail: fmt.Sprintf("no capture pattern within %d scan bytes", limit)}
            demuxer.emitPageError(perr)
            return false, perr
        }
        if finish {
            perr := &PageError{Offset: demuxer.checkOffset, Reason: ERR_REASON_EOF_TRUNCATED,
                Detail: fmt.Sprintf("stream ends with %d unrecoverable bytes", len(demuxer.checkBuf))}
            demuxer.emitPageError(perr)
            demuxer.dropCheckedBytes(len(demuxer.checkBuf))
            if demuxer.CheckMode == CHECK_STRICT {
                return false, perr
            }
        }
        // otherwise wait for more data; once more than MaxScanBytes are
        // buffered without a capture pattern the scan limit error fires
        return false, nil
    }
    drop := idx + 1
    demuxer.emitGap(&GapEvent{Offset: demuxer.checkOffset, DroppedBytes: drop, Reason: reason})
    demuxer.dropCheckedBytes(drop)
    return true, nil
}
