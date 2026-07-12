// Command mkbig writes a file of the given size filled with a repeating
// 0..250 byte pattern, matching the integration test's expectation.
package main

import (
	"log"
	"os"
	"strconv"
)

func main() {
	if len(os.Args) != 3 {
		log.Fatalf("usage: mkbig <path> <size>")
	}
	size, err := strconv.Atoi(os.Args[2])
	if err != nil {
		log.Fatal(err)
	}
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte(i % 251)
	}
	if err := os.WriteFile(os.Args[1], buf, 0o644); err != nil {
		log.Fatal(err)
	}
}
