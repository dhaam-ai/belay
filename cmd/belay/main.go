package main

import (
	"log"

	"github.com/belay-dev/belay/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		log.Fatal(err)
	}
}
