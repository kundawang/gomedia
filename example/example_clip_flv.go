package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/yapingcat/gomedia/go-flv"
)

// Export a [startMs, endMs] window of an FLV file into a standalone FLV
// without transcoding. The video start is moved backwards to the nearest
// keyframe and the AAC/AVC configuration tags are re-emitted so the result is
// independently decodable.
//
// Usage:
//
//	example_clip_flv input.flv output.flv startMs endMs
func main() {
	if len(os.Args) != 5 {
		fmt.Println("usage: example_clip_flv input.flv output.flv startMs endMs")
		return
	}
	startMs, err := strconv.ParseUint(os.Args[3], 10, 32)
	if err != nil {
		fmt.Println("invalid startMs:", err)
		return
	}
	endMs, err := strconv.ParseUint(os.Args[4], 10, 32)
	if err != nil {
		fmt.Println("invalid endMs:", err)
		return
	}

	stats, err := flv.ClipFlvFile(os.Args[1], os.Args[2], uint32(startMs), uint32(endMs))
	if err != nil {
		fmt.Println("clip failed:", err)
		return
	}
	fmt.Printf("clipped %d..%d ms (duration %d ms) videoTags=%d audioTags=%d "+
		"scriptTags=%d unknownTags=%d seekedKeyframe=%v videoConfig=%v audioConfig=%v metadataPatched=%v\n",
		stats.StartMs, stats.EndMs, stats.DurationMs,
		stats.VideoTags, stats.AudioTags, stats.ScriptTags, stats.UnknownTags,
		stats.SeekedKeyframe, stats.VideoConfigEmitted,
		stats.AudioConfigEmitted, stats.MetaDataPatched)
}
