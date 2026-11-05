// Command docgen writes the gateway configuration reference
// (docs/admin/configuration.md) from the doc comments on config.Config and
// the values of config.Default. Run it with `make generate-docs`.
package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strings"

	"github.com/solidDoWant/toolgate/internal/config"
)

const header = `# Gateway configuration reference

For: admins who deploy the toolgate gateway.

This page is generated from the code by ` + "`make generate-docs`" + `. Do not edit it by hand.

The gateway reads its configuration when ` + "`toolgate serve`" + ` starts, in this order:

1. The default value of each setting.
2. The YAML file named by the ` + "`--config`" + ` flag, or else by the ` + "`" + config.FileEnv + "`" + ` environment variable. The file is optional. An unknown key in the file is an error.
3. The environment variables. A variable that is set and not empty replaces the value from the file.

If a setting is missing or not valid, the gateway does not start. It prints one line for each problem and exits with code 1.

| YAML key | Environment variable | Default | Description |
|---|---|---|---|
`

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: docgen <config.go> <output.md>")
		os.Exit(2)
	}

	if err := generate(os.Args[1], os.Args[2]); err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
}

func generate(source, output string) error {
	comments, err := fieldComments(source)
	if err != nil {
		return err
	}

	var page bytes.Buffer

	page.WriteString(header)

	defaults := reflect.ValueOf(config.Default())
	fields := defaults.Type()

	for index := range fields.NumField() {
		field := fields.Field(index)

		comment, ok := comments[field.Name]
		if !ok {
			return fmt.Errorf("field %s has no doc comment", field.Name)
		}

		key := strings.Split(field.Tag.Get("json"), ",")[0]

		fmt.Fprintf(&page, "| `%s` | `%s` | %s | %s |\n",
			key, field.Tag.Get("env"), defaultText(defaults.Field(index)), comment)
	}

	return os.WriteFile(output, page.Bytes(), 0o644)
}

// fieldComments returns the doc comment of each field of the Config struct
// in source, joined into one line.
func fieldComments(source string) (map[string]string, error) {
	file, err := parser.ParseFile(token.NewFileSet(), source, nil, parser.ParseComments)
	if err != nil {
		return nil, err
	}

	comments := map[string]string{}

	ast.Inspect(file, func(node ast.Node) bool {
		spec, ok := node.(*ast.TypeSpec)
		if !ok || spec.Name.Name != "Config" {
			return true
		}

		structType, ok := spec.Type.(*ast.StructType)
		if !ok {
			return false
		}

		for _, field := range structType.Fields.List {
			for _, name := range field.Names {
				comments[name.Name] = strings.Join(strings.Fields(field.Doc.Text()), " ")
			}
		}

		return false
	})

	if len(comments) == 0 {
		return nil, fmt.Errorf("%s: no Config struct", source)
	}

	return comments, nil
}

func defaultText(value reflect.Value) string {
	if value.IsZero() {
		return "none"
	}

	encoded, err := json.Marshal(value.Interface())
	if err != nil {
		return "none"
	}

	var text string
	if err := json.Unmarshal(encoded, &text); err != nil {
		text = string(encoded)
	}

	return "`" + text + "`"
}
