package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/tinyrange/crumblecracker/internal/core/oci"
)

func main() {
	var chunkSize int
	var minChunkSize int
	flag.IntVar(&chunkSize, "chunk-size", 256<<10, "uncompressed bytes per large-file chunk")
	flag.IntVar(&minChunkSize, "min-chunk-size", 256<<10, "minimum bytes grouped into one gzip member")
	flag.Usage = func() {
		fmt.Fprintf(flag.CommandLine.Output(), "Usage: %s [options] INPUT-OCI-LAYOUT OUTPUT-OCI-LAYOUT\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	err := oci.RepackOCIImageLayout(context.Background(), flag.Arg(0), flag.Arg(1), chunkSize, minChunkSize, func(event oci.OCIImageRepackEvent) {
		fmt.Printf("manifest=%d layer=%d/%d input=%s output=%s size=%d members=%d\n",
			event.Manifest,
			event.Layer+1,
			event.Layers,
			event.InputDigest,
			event.Result.BlobDigest,
			event.Result.BlobSize,
			event.Result.Members,
		)
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "estargz-repack-image: %v\n", err)
		os.Exit(1)
	}
}
