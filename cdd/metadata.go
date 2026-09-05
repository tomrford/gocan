package cdd

import "strings"

// SourceIdentity retains the XML id, oid, temploid and QUAL of a CDD object.
// Missing attributes remain empty; qualifiers need not be unique. These values
// identify document objects and must not be used as UDS subfunctions.
type SourceIdentity struct {
	ID          string
	OID         string
	TemplateOID string
	Qualifier   string
}

// Metadata contains the presentation declared directly on an object. Template
// and datatype metadata are exposed separately; no text inheritance is inferred.
type Metadata struct {
	Source       SourceIdentity
	Names        LocalizedText
	Descriptions LocalizedText
}

// Translation is one TUV in document order. Text is plain text, retaining
// paragraph breaks and inline text order. Rich-text formatting is not retained.
type Translation struct {
	Language string
	Text     string
}

// LocalizedText retains all translations, including untagged text.
type LocalizedText []Translation

// Select tries language tags in caller-supplied preference order, comparing
// complete tags without case sensitivity. If none matches nonempty text, it
// returns the first nonempty translation in source order. It does not infer
// regional fallbacks: pass both "en-GB" and "en" to request that preference.
func (translations LocalizedText) Select(languages ...string) string {
	for _, language := range languages {
		for _, translation := range translations {
			if translation.Text != "" && strings.EqualFold(translation.Language, language) {
				return translation.Text
			}
		}
	}
	for _, translation := range translations {
		if translation.Text != "" {
			return translation.Text
		}
	}
	return ""
}

// DisplayName selects a declared name, falling back to the original qualifier.
// Display names do not change identifiers or codec field keys.
func (metadata Metadata) DisplayName(languages ...string) string {
	if name := metadata.Names.Select(languages...); name != "" {
		return name
	}
	return metadata.Source.Qualifier
}

func sourceIdentity(node *element) SourceIdentity {
	return SourceIdentity{
		ID: node.attr("id"), OID: node.attr("oid"),
		TemplateOID: node.attr("temploid"), Qualifier: node.childText("QUAL"),
	}
}

func metadata(node *element) Metadata {
	return Metadata{
		Source:       sourceIdentity(node),
		Names:        localizedText(node.child("NAME")),
		Descriptions: localizedText(node.child("DESC")),
	}
}

func localizedText(node *element) LocalizedText {
	var result LocalizedText
	for _, translation := range node.childrenNamed("TUV") {
		// Collapse XML indentation within a paragraph without joining paragraphs.
		var paragraphs []string
		for _, paragraph := range strings.Split(translation.text.String(), "\n\n") {
			if text := strings.Join(strings.Fields(paragraph), " "); text != "" {
				paragraphs = append(paragraphs, text)
			}
		}
		result = append(result, Translation{Language: translation.attr("lang"), Text: strings.Join(paragraphs, "\n\n")})
	}
	return result
}
