package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/adrianliechti/go-extract"
)

func runExtract(ctx context.Context, args []string) error {
	fs := newFlags("wingman extract <path>")

	positional, err := fs.ParseArgs(args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("expected exactly one path (run 'wingman extract --help' for usage)")
	}

	path := positional[0]

	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	doc, err := extract.Extract(ctx, extract.Input{
		Name: filepath.Base(path),
		Data: data,
	}, extract.Options{DiscardAttachmentData: true})
	if err != nil {
		return fmt.Errorf("extract %q: %w", path, err)
	}

	fmt.Println(doc.Markdown)
	return nil
}
