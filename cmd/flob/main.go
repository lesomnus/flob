package main

import (
	"context"
	"fmt"
	"os"

	"github.com/lesomnus/flob/cmd/flob/cmd"
)

func main() {
	c := cmd.NewCmdRoot()
	if err := c.Run(context.Background(), os.Args[1:]); err != nil {
		fmt.Println("app exited with error:", err)
		os.Exit(1)
	}
}
