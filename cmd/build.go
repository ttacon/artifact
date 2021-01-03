package main

import (
	"fmt"
	"log"

	"github.com/ttacon/artifact/builder"
	"github.com/urfave/cli/v2"
)

func build(c *cli.Context) error {
	log.Println("running sync")

	b := builder.NewBuilderFromCLI(c)

	targets, err := b.Run()
	if err != nil {
		return err
	}

	fmt.Println(targets)

	return nil
}
