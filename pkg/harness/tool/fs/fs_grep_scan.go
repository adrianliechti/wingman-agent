package fs

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"regexp"
	"strings"

	"github.com/adrianliechti/wingman-agent/pkg/text"
)

const maxGrepBufferedBytes = 10 * 1024 * 1024

func openTextFile(ctx context.Context, fsys fs.FS, path string) (io.ReadCloser, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	info, err := fs.Stat(fsys, path)
	if err != nil {
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		return nil, false, fmt.Errorf("path is not a regular file")
	}
	f, err := fsys.Open(path)
	if err != nil {
		return nil, false, err
	}
	reader := contextReader{ctx: ctx, source: f}
	var peek [binarySniffLen]byte
	n, err := io.ReadFull(reader, peek[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		f.Close()
		return nil, false, err
	}
	if isBinaryContent(peek[:n]) {
		f.Close()
		return nil, true, nil
	}
	return struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(peek[:n]), reader), f}, false, nil
}

func readGrepContent(ctx context.Context, source io.Reader) ([]byte, error) {
	data, err := readContent(ctx, source, maxGrepBufferedBytes)
	if err != nil {
		return nil, err
	}
	if len(data) > maxGrepBufferedBytes {
		return nil, fmt.Errorf("multiline and context searches are limited to %d MiB per file; narrow the search or use a command for larger files", maxGrepBufferedBytes/(1024*1024))
	}
	return data, nil
}

func grepScanner(source io.Reader) *bufio.Scanner {
	scanner := bufio.NewScanner(source)
	scanner.Buffer(make([]byte, 0, DefaultScanBufSize), MaxScanBufSize)
	return scanner
}

func grepScanError(ctx context.Context, scanner *bufio.Scanner) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if errors.Is(scanner.Err(), bufio.ErrTooLong) {
		return fmt.Errorf("line exceeds %d KiB scan limit; search is incomplete: %w", MaxScanBufSize/1024, scanner.Err())
	}
	return scanner.Err()
}

func fileHasMatch(ctx context.Context, fsys fs.FS, path string, re *regexp.Regexp, multiline bool) (bool, error) {
	rc, binary, err := openTextFile(ctx, fsys, path)
	if err != nil || binary {
		return false, err
	}
	defer rc.Close()
	if multiline {
		data, err := readGrepContent(ctx, rc)
		if err != nil {
			return false, err
		}
		return re.Match(data), ctx.Err()
	}
	scanner := grepScanner(rc)
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if re.Match(scanner.Bytes()) {
			return true, nil
		}
	}
	return false, grepScanError(ctx, scanner)
}

func countFileMatches(ctx context.Context, fsys fs.FS, path string, re *regexp.Regexp, multiline bool) (int, error) {
	rc, binary, err := openTextFile(ctx, fsys, path)
	if err != nil || binary {
		return 0, err
	}
	defer rc.Close()
	if multiline {
		data, err := readGrepContent(ctx, rc)
		if err != nil {
			return 0, err
		}
		return len(re.FindAllIndex(data, -1)), ctx.Err()
	}
	scanner := grepScanner(rc)
	count := 0
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		count += len(re.FindAllIndex(scanner.Bytes(), -1))
	}
	return count, grepScanError(ctx, scanner)
}

func grepDisplayLine(line string) string {
	if len(line) > MaxLineDisplayLength {
		return text.HeadBytes(line, MaxLineDisplayLength-3) + "..."
	}
	return line
}

func searchFileWithContext(ctx context.Context, fsys fs.FS, path, display string, cfg *grepArgs, emit func(string) bool) error {
	rc, binary, err := openTextFile(ctx, fsys, path)
	if err != nil || binary {
		return err
	}
	defer rc.Close()
	if !cfg.multiline && cfg.beforeContext == 0 && cfg.afterContext == 0 {
		scanner := grepScanner(rc)
		line := 0
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
			line++
			if cfg.re.Match(scanner.Bytes()) && !emit(formatGrepLine(display, line, true, grepDisplayLine(scanner.Text()), cfg.showLineNumbers)) {
				return errGrepPageFull
			}
		}
		return grepScanError(ctx, scanner)
	}

	data, err := readGrepContent(ctx, rc)
	if err != nil {
		return err
	}
	var lines []string
	var matched []bool
	if cfg.multiline {
		// Match the original bytes, including CRLF and the final newline. Only
		// display formatting removes line terminators.
		full := string(data)
		lines = strings.Split(full, "\n")
		if strings.HasSuffix(full, "\n") {
			lines = lines[:len(lines)-1]
		}
		matched = make([]bool, len(lines))
		starts := make([]int, len(lines))
		offset := 0
		for i, line := range lines {
			starts[i] = offset
			offset += len(line) + 1
		}
		for _, span := range cfg.re.FindAllStringIndex(full, -1) {
			if err := ctx.Err(); err != nil {
				return err
			}
			first := lineContaining(starts, span[0])
			last := lineContaining(starts, max(span[0], span[1]-1))
			for i := first; i <= last; i++ {
				matched[i] = true
			}
		}
	} else {
		scanner := grepScanner(bytes.NewReader(data))
		for scanner.Scan() {
			if err := ctx.Err(); err != nil {
				return err
			}
			line := scanner.Text()
			lines = append(lines, line)
			matched = append(matched, cfg.re.MatchString(line))
		}
		if err := grepScanError(ctx, scanner); err != nil {
			return err
		}
	}

	lastPrinted := -1
	for i := range lines {
		if err := ctx.Err(); err != nil {
			return err
		}
		if !matched[i] {
			continue
		}
		start := max(0, i-cfg.beforeContext)
		end := i + min(cfg.afterContext, len(lines)-1-i)
		if lastPrinted >= 0 && start > lastPrinted+1 && !emit("--") {
			return errGrepPageFull
		}
		for j := max(start, lastPrinted+1); j <= end; j++ {
			line := grepDisplayLine(strings.TrimSuffix(lines[j], "\r"))
			if !emit(formatGrepLine(display, j+1, matched[j], line, cfg.showLineNumbers)) {
				return errGrepPageFull
			}
			lastPrinted = j
		}
	}
	return ctx.Err()
}
