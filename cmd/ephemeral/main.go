package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/lessucettes/ephemeral/internal/session"
	"github.com/lessucettes/ephemeral/internal/terminal"
)

var version = "dev"

func main() {
	versionFlag := flag.Bool("version", false, "Print the version and exit")
	vFlag := flag.Bool("v", false, "Print the version and exit (shorthand)")
	flag.Parse()

	if *versionFlag || *vFlag {
		fmt.Println(version)
		os.Exit(0)
	}

	intents := make(chan session.InboundIntent, 10)
	updates := make(chan session.SurfaceUpdate, 10)

	engine, err := session.NewEngine(intents, updates)
	if err != nil {
		log.Fatalf("Failed to create session engine: %v", err)
	}

	ui := terminal.NewConsole(intents, updates)

	go engine.Run()

	if err := ui.Run(); err != nil {
		log.Fatalf("Failed to run terminal UI: %v", err)
	}
}
