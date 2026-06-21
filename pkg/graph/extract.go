package graph

import (
	"path"
	"strings"

	ts "github.com/odvcencio/gotreesitter"
)

var importQueries = map[string]string{
	"go": `(import_spec path: (interpreted_string_literal) @path)`,

	"python": `(import_statement (dotted_name) @path)
(import_statement (aliased_import (dotted_name) @path))
(import_from_statement module_name: (dotted_name) @path)
(import_from_statement module_name: (relative_import) @rel)`,

	"javascript": `(import_statement source: (string) @path)
(export_statement source: (string) @path)`,

	"typescript": `(import_statement source: (string) @path)
(export_statement source: (string) @path)`,

	"tsx": `(import_statement source: (string) @path)
(export_statement source: (string) @path)`,
}

var tagsAugment = map[string]string{
	"go": `(type_spec name: (type_identifier) @name) @definition.type`,
}

var hierarchyQueries = map[string]string{
	"go": `(field_declaration !name (type_identifier) @inherits)
(field_declaration !name (qualified_type name: (type_identifier) @inherits))
(interface_type (type_elem (type_identifier) @inherits))`,

	"python": `(class_definition superclasses: (argument_list (identifier) @inherits))
(class_definition superclasses: (argument_list (attribute attribute: (identifier) @inherits)))`,

	"javascript": `(class_heritage (identifier) @inherits)
(class_heritage (member_expression property: (property_identifier) @inherits))`,

	"typescript": `(extends_clause (identifier) @inherits)
(extends_clause (member_expression property: (property_identifier) @inherits))
(implements_clause (type_identifier) @implements)
(extends_type_clause (type_identifier) @inherits)`,

	"tsx": `(extends_clause (identifier) @inherits)
(extends_clause (member_expression property: (property_identifier) @inherits))
(implements_clause (type_identifier) @implements)
(extends_type_clause (type_identifier) @inherits)`,
}

type rawImport struct {
	norm string
	rel  bool
}

type rawHierRef struct {
	name      string
	kind      EdgeKind
	startByte uint32
}

type auxExtractor struct {
	parsers map[string]*ts.Parser
	importQ map[string]*ts.Query
	hierQ   map[string]*ts.Query
}

func newAuxExtractor() *auxExtractor {
	return &auxExtractor{
		parsers: map[string]*ts.Parser{},
		importQ: map[string]*ts.Query{},
		hierQ:   map[string]*ts.Query{},
	}
}

func (ax *auxExtractor) query(lang string, langObj *ts.Language, srcs map[string]string, cache map[string]*ts.Query) *ts.Query {
	if q, ok := cache[lang]; ok {
		return q
	}
	var q *ts.Query
	if s := srcs[lang]; s != "" {
		if compiled, err := ts.NewQuery(s, langObj); err == nil {
			q = compiled
		}
	}
	cache[lang] = q
	return q
}

func (ax *auxExtractor) extract(lang string, langObj *ts.Language, src []byte) ([]rawImport, []rawHierRef) {
	iq := ax.query(lang, langObj, importQueries, ax.importQ)
	hq := ax.query(lang, langObj, hierarchyQueries, ax.hierQ)
	if iq == nil && hq == nil {
		return nil, nil
	}

	parser := ax.parsers[lang]
	if parser == nil {
		parser = ts.NewParser(langObj)
		ax.parsers[lang] = parser
	}
	tree, err := parser.Parse(src)
	if err != nil {
		return nil, nil
	}
	root := tree.RootNode()

	var imps []rawImport
	if iq != nil {
		cur := iq.Exec(root, langObj, src)
		for {
			m, ok := cur.NextMatch()
			if !ok {
				break
			}
			for _, cap := range m.Captures {
				text := cap.Node.Text(src)
				var norm string
				var rel bool
				if lang == "python" {
					rel = cap.Name == "rel"
					norm = normalizePython(text, rel)
				} else {
					s := strings.Trim(text, "\"'`")
					rel = strings.HasPrefix(s, ".")
					norm = s
				}
				if norm != "" {
					imps = append(imps, rawImport{norm: norm, rel: rel})
				}
			}
		}
	}

	var hiers []rawHierRef
	if hq != nil {
		cur := hq.Exec(root, langObj, src)
		for {
			m, ok := cur.NextMatch()
			if !ok {
				break
			}
			for _, cap := range m.Captures {
				kind := EdgeInherits
				if cap.Name == "implements" {
					kind = EdgeImplements
				}
				hiers = append(hiers, rawHierRef{name: cap.Node.Text(src), kind: kind, startByte: cap.Node.StartByte()})
			}
		}
	}

	return imps, hiers
}

func normalizePython(text string, rel bool) string {
	if !rel {
		return strings.ReplaceAll(text, ".", "/")
	}
	level := 0
	for level < len(text) && text[level] == '.' {
		level++
	}
	rest := strings.ReplaceAll(text[level:], ".", "/")
	return strings.Repeat("../", level-1) + rest
}

func resolveImport(fromFile, norm string, rel bool, localDirs map[string]bool) string {
	if rel {
		base := path.Dir(fromFile)
		target := path.Clean(path.Join(base, norm))
		if localDirs[target] {
			return target
		}
		if d := path.Dir(target); localDirs[d] {
			return d
		}
		return ""
	}

	best := ""
	for d := range localDirs {
		if d == "." {
			continue
		}
		if norm == d || strings.HasSuffix(norm, "/"+d) {
			if len(d) > len(best) {
				best = d
			}
		}
	}
	return best
}
