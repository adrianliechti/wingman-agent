package lsp

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"unicode"

	"github.com/adrianliechti/wingman-agent/pkg/fileuri"
	"github.com/adrianliechti/wingman-agent/pkg/language"
	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

type symbolCandidate struct {
	name      string
	qualified string
	position  lsp.Position
}

func positionFromDisplay(path string, line, column int) (lsp.Position, error) {
	lines, err := readLines(path)
	if err != nil {
		return lsp.Position{}, err
	}
	if line < 1 || line > len(lines) {
		return lsp.Position{}, fmt.Errorf("line %d out of range (file has %d lines)", line, len(lines))
	}
	runes := []rune(lines[line-1])
	if column < 1 || column > len(runes)+1 {
		return lsp.Position{}, fmt.Errorf("column %d out of range for line %d (line has %d columns)", column, line, len(runes)+1)
	}
	return lsp.Position{Line: uint32(line - 1), Character: uint32(utf16ColumnFromRune(lines[line-1], column-1))}, nil
}

func positionOfSymbolOnLine(path string, line int, symbol string) (lsp.Position, error) {
	lines, err := readLines(path)
	if err != nil {
		return lsp.Position{}, err
	}
	if line < 1 || line > len(lines) {
		return lsp.Position{}, fmt.Errorf("line %d out of range (file has %d lines)", line, len(lines))
	}
	column := findWordInLine(lines[line-1], symbol)
	if column < 0 {
		return lsp.Position{}, fmt.Errorf("symbol %q not found on line %d", symbol, line)
	}
	if findWordInLineFrom(lines[line-1], symbol, column+1) >= 0 {
		return lsp.Position{}, fmt.Errorf("symbol %q is ambiguous on line %d; pass line and column", symbol, line)
	}
	return lsp.Position{Line: uint32(line - 1), Character: uint32(utf16ColumnFromRune(lines[line-1], column))}, nil
}

func symbolPosition(ctx context.Context, session *lsp.Session, uri, name string, line int) (lsp.Position, error) {
	path, _ := fileuri.Path(uri)
	var result lsp.DocumentSymbolResult
	var err error
	if lsp.CapabilityEnabled(session.Capabilities().DocumentSymbolProvider) {
		result, err = session.DocumentSymbols(ctx, uri)
		if err != nil {
			return lsp.Position{}, fmt.Errorf("symbol lookup failed: %w. %s", err, fallbackAdvice)
		}
	}
	documentSymbols, symbols := language.DocumentSymbolsFromProtocol(result)
	if err == nil {
		var candidates []symbolCandidate
		for _, symbol := range symbols {
			candidates = append(candidates, symbolCandidate{name: symbol.Name, qualified: symbol.Name, position: symbol.Location.Range.Start})
		}
		collectSymbolCandidates(documentSymbols, "", &candidates)
		if line > 0 {
			candidates = slices.DeleteFunc(candidates, func(c symbolCandidate) bool { return int(c.position.Line)+1 != line })
		}
		matched, matchErr := matchSymbol(candidates, name)
		if matchErr != nil {
			return lsp.Position{}, matchErr
		}
		if len(matched) == 1 {
			return snapToIdentifier(path, matched[0]), nil
		}
	}
	if err := ctx.Err(); err != nil {
		return lsp.Position{}, err
	}
	// A qualified name must not silently resolve to an unrelated leaf name.
	if strings.Contains(name, ".") {
		return lsp.Position{}, fmt.Errorf("qualified symbol %q not found; use documentSymbol or pass line and column", name)
	}
	return positionOfSymbol(path, name)
}

func snapToIdentifier(path string, matched symbolCandidate) lsp.Position {
	position := matched.position
	lines, err := readLines(path)
	if err != nil || int(position.Line) >= len(lines) {
		return position
	}
	text := lines[position.Line]
	if index := findWordInLineFrom(text, symbolLeaf(matched.name), runeColumnFromUTF16(text, int(position.Character))); index >= 0 {
		position.Character = uint32(utf16ColumnFromRune(text, index))
	}
	return position
}

func collectSymbolCandidates(symbols []lsp.DocumentSymbol, prefix string, result *[]symbolCandidate) {
	for _, symbol := range symbols {
		qualified := symbol.Name
		if prefix != "" {
			qualified = prefix + "." + symbol.Name
		}
		*result = append(*result, symbolCandidate{name: symbol.Name, qualified: qualified, position: symbol.SelectionRange.Start})
		collectSymbolCandidates(symbol.Children, qualified, result)
	}
}

func matchSymbol(candidates []symbolCandidate, query string) ([]symbolCandidate, error) {
	var matches []symbolCandidate
	normalized := normalizeSymbol(query)
	for _, candidate := range candidates {
		qualified, name := normalizeSymbol(candidate.qualified), normalizeSymbol(candidate.name)
		matched := qualified == normalized || name == normalized
		if strings.Contains(query, ".") {
			matched = matched || strings.HasSuffix(qualified, "."+normalized) || strings.HasSuffix(name, "."+normalized)
		} else {
			matched = matched || symbolLeaf(candidate.name) == query
		}
		if matched && !slices.ContainsFunc(matches, func(other symbolCandidate) bool { return other.position == candidate.position }) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) > 1 {
		var choices []string
		for _, candidate := range matches[:min(len(matches), 10)] {
			choices = append(choices, fmt.Sprintf("%s (line %d)", candidate.qualified, candidate.position.Line+1))
		}
		return nil, fmt.Errorf("symbol %q is ambiguous (%d matches): %s; pass a qualified symbol or line and column", query, len(matches), strings.Join(choices, ", "))
	}
	return matches, nil
}

func positionOfSymbol(path, symbol string) (lsp.Position, error) {
	lines, err := readLines(path)
	if err != nil {
		return lsp.Position{}, err
	}
	var matches []symbolCandidate
	for line, text := range lines {
		for column := findWordInLine(text, symbol); column >= 0; column = findWordInLineFrom(text, symbol, column+1) {
			matches = append(matches, symbolCandidate{name: symbol, qualified: symbol, position: lsp.Position{Line: uint32(line), Character: uint32(utf16ColumnFromRune(text, column))}})
		}
	}
	matched, err := matchSymbol(matches, symbol)
	if err != nil {
		return lsp.Position{}, err
	}
	if len(matched) == 0 {
		return lsp.Position{}, fmt.Errorf("symbol %q not found", symbol)
	}
	return matched[0].position, nil
}

func readLines(path string) ([]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	lines := strings.Split(string(data), "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines, nil
}

func utf16ColumnFromRune(line string, runeColumn int) int {
	column := 0
	for index, value := range []rune(line) {
		if index >= runeColumn {
			break
		}
		if value > 0xffff {
			column += 2
		} else {
			column++
		}
	}
	return column
}

func runeColumnFromUTF16(line string, utf16Column int) int {
	column, runes := 0, 0
	for _, value := range line {
		if column >= utf16Column {
			break
		}
		if value > 0xffff {
			column += 2
		} else {
			column++
		}
		runes++
	}
	return runes
}

func findWordInLine(line, word string) int { return findWordInLineFrom(line, word, 0) }

func findWordInLineFrom(line, word string, start int) int {
	if word == "" {
		return -1
	}
	lineRunes, wordRunes := []rune(line), []rune(word)
	for index := max(start, 0); index+len(wordRunes) <= len(lineRunes); index++ {
		if string(lineRunes[index:index+len(wordRunes)]) != word {
			continue
		}
		if index > 0 && isWordCharacter(lineRunes[index-1]) || index+len(wordRunes) < len(lineRunes) && isWordCharacter(lineRunes[index+len(wordRunes)]) {
			continue
		}
		return index
	}
	return -1
}

func isWordCharacter(value rune) bool {
	return value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value)
}

func symbolLeaf(name string) string {
	name = strings.TrimSuffix(name, "()")
	if index := strings.LastIndex(name, "."); index >= 0 {
		name = name[index+1:]
	}
	return strings.Trim(name, "*()")
}

func normalizeSymbol(name string) string {
	return strings.Map(func(value rune) rune {
		switch value {
		case '(', ')', '*', ' ':
			return -1
		default:
			return value
		}
	}, name)
}
