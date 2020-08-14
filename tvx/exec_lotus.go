package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/lib/blockstore"
	"github.com/google/brotli/go/cbrotli"
	"github.com/ipld/go-car"
	"github.com/urfave/cli/v2"

	"github.com/filecoin-project/oni/tvx/lotus"
	"github.com/filecoin-project/oni/tvx/schema"
)

var execLotusFlags struct {
	file string
}

var execLotusCmd = &cli.Command{
	Name:        "exec-lotus",
	Description: "execute a test vector against Lotus",
	Flags: []cli.Flag{
		&cli.StringFlag{
			Name:        "file",
			Usage:       "input file",
			Destination: &execLotusFlags.file,
		},
	},
	Action: runExecLotus,
}

func runExecLotus(_ *cli.Context) error {
	switch {
	case execLotusFlags.file != "":
		file, err := os.Open(execLotusFlags.file)
		if err != nil {
			return fmt.Errorf("failed to open test vector: %w", err)
		}

		var (
			dec = json.NewDecoder(file)
			tv  schema.TestVector
		)

		if err = dec.Decode(&tv); err != nil {
			return fmt.Errorf("failed to decode test vector: %w", err)
		}

		return executeTestVector(tv)
	default:
		dec := json.NewDecoder(os.Stdin)
		for {
			var tv schema.TestVector

			err := dec.Decode(&tv)
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}

			err = executeTestVector(tv)
			if err != nil {
				return err
			}
		}
	}
}

func executeTestVector(tv schema.TestVector) error {
	fmt.Println("executing test vector:", tv.Meta.Desc)
	switch tv.Class {
	case "message":
		var (
			ctx   = context.Background()
			epoch = tv.Pre.Epoch
			root  = tv.Pre.StateTree.RootCID
		)

		bs := blockstore.NewTemporary()

		buf := bytes.NewReader(tv.CAR)
		br := cbrotli.NewReader(buf)
		defer br.Close()

		header, err := car.LoadCar(bs, br)
		if err != nil {
			return fmt.Errorf("failed to load state tree car from test vector: %w", err)
		}

		fmt.Println("roots: ", header.Roots)

		driver := lotus.NewDriver(ctx)

		for i, m := range tv.ApplyMessages {
			fmt.Printf("decoding message %v\n", i)
			msg, err := types.DecodeMessage(m.Bytes)
			if err != nil {
				return err
			}

			// add an epoch if we have set one
			if m.Epoch != nil {
				epoch = *m.Epoch
			}

			fmt.Printf("executing message %v\n", i)
			_, root, err = driver.ExecuteMessage(msg, root, bs, epoch)
			if err != nil {
				return err
			}
		}

		if root != tv.Post.StateTree.RootCID {
			return fmt.Errorf("wrong post root cid; expected %v , but got %v", tv.Post.StateTree.RootCID, root)
		}

		return nil

	default:
		return fmt.Errorf("test vector class not supported")
	}
}
