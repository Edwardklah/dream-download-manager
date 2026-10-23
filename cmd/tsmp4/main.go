// tsmp4 converts an MPEG-TS file to MP4 with package mpegts - the same code
// the app uses for HLS videos. For testing a piece of a video on its own:
//
//	go run ./cmd/tsmp4 piece.ts out.mp4
package main

import (
	"fmt"
	"os"

	"dlmanager/mpegts"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: tsmp4 in.ts out.mp4")
		os.Exit(2)
	}
	if err := mpegts.ToMP4(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "tsmp4:", err)
		os.Exit(1)
	}
	fmt.Println("converted", os.Args[1], "->", os.Args[2])
}
