package language

import (
	"context"
	"errors"
	"fmt"

	"github.com/adrianliechti/wingman-agent/pkg/lsp"
)

func (s *Service) DefinitionLocations(ctx context.Context, filePath string, content *string, line, column int) ([]lsp.Location, error) {
	return s.structuralLocations(
		ctx, filePath, content, line, column,
		func(capabilities lsp.ServerCapabilities) any { return capabilities.DefinitionProvider },
		func(session *lsp.Session, uri string) ([]lsp.Location, error) {
			return session.DefinitionLocations(ctx, uri, line, column)
		},
		s.TreeSitterDefinitions,
	)
}

func (s *Service) TypeDefinitionLocations(ctx context.Context, filePath string, content *string, line, column int) ([]lsp.Location, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) ([]lsp.Location, error) {
		return session.TypeDefinitionLocations(ctx, uri, line, column)
	})
}

func (s *Service) ImplementationLocations(ctx context.Context, filePath string, content *string, line, column int) ([]lsp.Location, error) {
	return s.structuralLocations(
		ctx, filePath, content, line, column,
		func(capabilities lsp.ServerCapabilities) any { return capabilities.ImplementationProvider },
		func(session *lsp.Session, uri string) ([]lsp.Location, error) {
			return session.ImplementationLocations(ctx, uri, line, column)
		},
		s.TreeSitterImplementations,
	)
}

func (s *Service) ReferenceLocations(ctx context.Context, filePath string, content *string, line, column int, includeDeclaration bool) ([]lsp.Location, error) {
	return s.structuralLocations(
		ctx, filePath, content, line, column,
		func(capabilities lsp.ServerCapabilities) any { return capabilities.ReferencesProvider },
		func(session *lsp.Session, uri string) ([]lsp.Location, error) {
			return session.ReferenceLocations(ctx, uri, line, column, includeDeclaration)
		},
		func(ctx context.Context, filePath string, content *string, line, column int) ([]lsp.Location, error) {
			locations, _, err := s.TreeSitterReferences(ctx, filePath, content, line, column, includeDeclaration)
			return locations, err
		},
	)
}

func (s *Service) structuralLocations(
	ctx context.Context,
	filePath string,
	content *string,
	line, column int,
	capability func(lsp.ServerCapabilities) any,
	request func(*lsp.Session, string) ([]lsp.Location, error),
	fallback func(context.Context, string, *string, int, int) ([]lsp.Location, error),
) ([]lsp.Location, error) {
	locations, err := withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) ([]lsp.Location, error) {
		if !lsp.CapabilityEnabled(capability(session.Capabilities())) {
			return nil, errors.ErrUnsupported
		}
		return request(session, uri)
	})
	if err == nil {
		return locations, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return fallback(ctx, filePath, content, line, column)
}

func (s *Service) HoverRange(ctx context.Context, filePath string, content *string, line, column int) (string, *lsp.Range, error) {
	hover, err := withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) (*lsp.Hover, error) {
		if !lsp.CapabilityEnabled(session.Capabilities().HoverProvider) {
			return nil, errors.ErrUnsupported
		}
		return session.Hover(ctx, uri, line, column)
	})
	if err == nil {
		if hover == nil {
			return "", nil, nil
		}
		return HoverText(hover), hover.Range, nil
	}
	if ctx.Err() != nil {
		return "", nil, ctx.Err()
	}
	text, err := s.TreeSitterHover(ctx, filePath, content, line, column)
	return text, nil, err
}

func (s *Service) CompletionItems(ctx context.Context, filePath string, content *string, line, column int, completionContext *lsp.CompletionContext) (lsp.CompletionList, error) {
	list, err := withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) (lsp.CompletionList, error) {
		if session.Capabilities().CompletionProvider == nil {
			return lsp.CompletionList{}, errors.ErrUnsupported
		}
		return session.CompletionItems(ctx, uri, line, column, completionContext)
	})
	if err == nil {
		return list, nil
	}
	if ctx.Err() != nil {
		return lsp.CompletionList{}, ctx.Err()
	}
	if completionContext != nil && completionContext.TriggerCharacter != nil {
		return lsp.CompletionList{Items: []lsp.CompletionItem{}}, nil
	}
	items, err := s.treeSitterCompletionItems(filePath, content)
	return lsp.CompletionList{Items: items}, err
}

func (s *Service) ResolveCompletionItem(ctx context.Context, filePath string, content *string, item lsp.CompletionItem) (lsp.CompletionItem, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, _ string) (lsp.CompletionItem, error) {
		return session.ResolveCompletionItem(ctx, item)
	})
}

func (s *Service) SignatureHelp(ctx context.Context, filePath string, content *string, line, column int, signatureContext *lsp.SignatureHelpContext) (*lsp.SignatureHelp, error) {
	if !s.HasLSPFor(filePath) {
		return nil, nil
	}
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) (*lsp.SignatureHelp, error) {
		return session.SignatureHelp(ctx, uri, line, column, signatureContext)
	})
}

func (s *Service) PrepareRename(ctx context.Context, filePath string, content *string, line, column int) (lsp.PrepareRenameResult, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) (lsp.PrepareRenameResult, error) {
		return session.PrepareRename(ctx, uri, line, column)
	})
}

func (s *Service) Rename(ctx context.Context, filePath string, content *string, line, column int, newName string) (*lsp.WorkspaceEdit, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) (*lsp.WorkspaceEdit, error) {
		return session.Rename(ctx, uri, line, column, newName)
	})
}

func (s *Service) CodeActions(ctx context.Context, filePath string, content *string, selection lsp.Range, only []lsp.CodeActionKind, trigger lsp.CodeActionTriggerKind) ([]lsp.CommandOrCodeAction, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) ([]lsp.CommandOrCodeAction, error) {
		return session.CodeActions(ctx, uri, selection, only, trigger)
	})
}

func (s *Service) ResolveCodeAction(ctx context.Context, filePath string, content *string, action lsp.CodeAction) (*lsp.CodeAction, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, _ string) (*lsp.CodeAction, error) {
		return session.ResolveCodeAction(ctx, action)
	})
}

func (s *Service) ExecuteCommand(ctx context.Context, filePath string, content *string, command lsp.Command, apply func(lsp.CommandWorkspaceEdit) bool) (any, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, _ string) (any, error) {
		return session.ExecuteCommand(ctx, command, apply)
	})
}

func (s *Service) Formatting(ctx context.Context, filePath string, content *string, options lsp.FormattingOptions) ([]lsp.TextEdit, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) ([]lsp.TextEdit, error) {
		return session.Formatting(ctx, uri, options)
	})
}

func (s *Service) RangeFormatting(ctx context.Context, filePath string, content *string, selection lsp.Range, options lsp.FormattingOptions) ([]lsp.TextEdit, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) ([]lsp.TextEdit, error) {
		return session.RangeFormatting(ctx, uri, selection, options)
	})
}

func (s *Service) OnTypeFormatting(ctx context.Context, filePath string, content *string, line, column int, character string, options lsp.FormattingOptions) ([]lsp.TextEdit, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) ([]lsp.TextEdit, error) {
		return session.OnTypeFormatting(ctx, uri, line, column, character, options)
	})
}

func (s *Service) InlayHints(ctx context.Context, filePath string, content *string, selection lsp.Range) ([]lsp.InlayHint, error) {
	return withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) ([]lsp.InlayHint, error) {
		return session.InlayHints(ctx, uri, selection)
	})
}

func (s *Service) DocumentSymbols(ctx context.Context, filePath string, content *string) ([]lsp.DocumentSymbol, []lsp.SymbolInformation, error) {
	result, err := withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) (lsp.DocumentSymbolResult, error) {
		if !lsp.CapabilityEnabled(session.Capabilities().DocumentSymbolProvider) {
			return nil, errors.ErrUnsupported
		}
		return session.DocumentSymbols(ctx, uri)
	})
	if err == nil {
		documents, flat := DocumentSymbolsFromProtocol(result)
		return documents, flat, nil
	}
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	symbols, err := s.TreeSitterDocumentSymbols(filePath, content)
	return symbols, nil, err
}

func (s *Service) DocumentHighlights(ctx context.Context, filePath string, content *string, line, column int) ([]lsp.DocumentHighlight, error) {
	highlights, err := withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) ([]lsp.DocumentHighlight, error) {
		if !lsp.CapabilityEnabled(session.Capabilities().DocumentHighlightProvider) {
			return nil, errors.ErrUnsupported
		}
		return session.DocumentHighlights(ctx, uri, line, column)
	})
	if err == nil {
		return highlights, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return s.treeSitterDocumentHighlights(filePath, content, line, column)
}

func (s *Service) FoldingRanges(ctx context.Context, filePath string, content *string) ([]lsp.FoldingRange, error) {
	ranges, err := withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) ([]lsp.FoldingRange, error) {
		if !lsp.CapabilityEnabled(session.Capabilities().FoldingRangeProvider) {
			return nil, errors.ErrUnsupported
		}
		return session.FoldingRanges(ctx, uri)
	})
	if err == nil {
		return ranges, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return s.treeSitterFoldingRanges(filePath, content)
}

func (s *Service) SemanticTokens(ctx context.Context, filePath string, content *string) ([]SemanticToken, error) {
	var legend lsp.SemanticTokensLegend
	tokens, err := withLSPDocument(ctx, s, filePath, content, func(session *lsp.Session, uri string) (*lsp.SemanticTokens, error) {
		if !lsp.CapabilityEnabled(session.Capabilities().SemanticTokensProvider) {
			return nil, errors.ErrUnsupported
		}
		legend = semanticTokenLegend(session.Capabilities())
		return session.SemanticTokens(ctx, uri)
	})
	if err == nil && tokens != nil {
		return decodeSemanticTokens(tokens.Data, legend)
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return s.treeSitterSemanticTokens(filePath, content)
}

func semanticTokenLegend(capabilities lsp.ServerCapabilities) lsp.SemanticTokensLegend {
	switch options := capabilities.SemanticTokensProvider.(type) {
	case *lsp.SemanticTokensOptions:
		return options.Legend
	case *lsp.SemanticTokensRegistrationOptions:
		return options.Legend
	default:
		return lsp.SemanticTokensLegend{}
	}
}

func decodeSemanticTokens(data []uint32, legend lsp.SemanticTokensLegend) ([]SemanticToken, error) {
	if len(data)%5 != 0 {
		return nil, fmt.Errorf("invalid semantic token data length %d", len(data))
	}

	tokens := make([]SemanticToken, 0, len(data)/5)
	line, character := 0, 0
	for index := 0; index < len(data); index += 5 {
		lineDelta := int(data[index])
		characterDelta := int(data[index+1])
		line += lineDelta
		if lineDelta == 0 {
			character += characterDelta
		} else {
			character = characterDelta
		}
		typeIndex := int(data[index+3])
		if typeIndex >= len(legend.TokenTypes) {
			continue
		}
		modifierBits := data[index+4]
		modifiers := make([]string, 0)
		for modifierIndex, modifier := range legend.TokenModifiers {
			if modifierIndex < 32 && modifierBits&(1<<modifierIndex) != 0 {
				modifiers = append(modifiers, modifier)
			}
		}
		tokens = append(tokens, SemanticToken{
			Line:      line,
			Character: character,
			Length:    int(data[index+2]),
			Type:      legend.TokenTypes[typeIndex],
			Modifiers: modifiers,
		})
	}
	return tokens, nil
}
