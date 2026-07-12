// Command goafp is a client for the Apple Filing Protocol.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/lex/goafp/internal/afp"
	"github.com/lex/goafp/internal/dsi"
)

const usage = `usage: goafp <command> [arguments]

Commands:
  status <host[:port]>   query an AFP server without authenticating
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "status":
		if len(os.Args) != 3 {
			fmt.Fprint(os.Stderr, usage)
			os.Exit(2)
		}
		err = runStatus(os.Args[2])
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "goafp: %v\n", err)
		os.Exit(1)
	}
}

func runStatus(addr string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, err := dsi.Dial(ctx, addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	block, err := conn.GetStatus(ctx)
	if err != nil {
		return err
	}
	info, err := afp.ParseServerInfo(block)
	if err != nil {
		return err
	}

	fmt.Printf("Server name:  %s\n", info.ServerName)
	fmt.Printf("Machine type: %s\n", info.MachineType)
	fmt.Printf("AFP versions: %s\n", strings.Join(info.AFPVersions, ", "))
	fmt.Printf("UAMs:         %s\n", strings.Join(info.UAMs, ", "))
	fmt.Printf("Flags:        %#04x\n", info.Flags)
	return nil
}
