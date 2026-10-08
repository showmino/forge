package template

import (
	"strings"
	gotemplate "text/template"
)

// funcs is the whole function set templates may use: nothing reads files,
// runs commands or reaches the network.
var funcs = gotemplate.FuncMap{
	"lower":      strings.ToLower,
	"upper":      strings.ToUpper,
	"replace":    func(old, repl, s string) string { return strings.ReplaceAll(s, old, repl) },
	"trimPrefix": func(prefix, s string) string { return strings.TrimPrefix(s, prefix) },
	"trimSuffix": func(suffix, s string) string { return strings.TrimSuffix(s, suffix) },
}

func (t *Template) parse(name, text string) (*gotemplate.Template, error) {
	left, right := t.Delims()
	return gotemplate.New(name).Delims(left, right).Option("missingkey=error").Funcs(funcs).Parse(text)
}

func (t *Template) renderString(name, text string, v Values) (string, error) {
	tp, err := t.parse(name, text)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if err := tp.Execute(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}
