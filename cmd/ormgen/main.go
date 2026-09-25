// Command ormgen generates typed field descriptors for Lemmego ORM models.
//
// The descriptors are the compile-time-checked half of the ORM's query API:
//
//	db.Model[User]().Where(UserFields.Active.Eq(true))
//
// which is equivalent to, and compiles to the same SQL as, the string form
// orm.Eq("active", true). Generating them is optional; the ORM is fully usable
// without ever running this tool.
//
// Usage:
//
//	ormgen [flags] [directory]
//
//	-out string      output file name (default "orm_fields.go")
//	-type string     comma-separated type names to generate for
//	-all             generate for every exported struct, not just tagged ones
//	-package string  override the detected package name
//	-stdout          write to standard output instead of a file
//
// Typically invoked through go:generate:
//
//	//go:generate go run github.com/lemmego/orm/cmd/ormgen .
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/lemmego/orm/ormgen"
)

func main() {
	out := flag.String("out", ormgen.OutputFileName, "output file name, written inside the target directory")
	typeList := flag.String("type", "", "comma-separated type names to generate for")
	all := flag.Bool("all", false, "generate for every exported struct, not just those carrying orm or db tags")
	packageName := flag.String("package", "", "override the detected package name")
	toStdout := flag.Bool("stdout", false, "write to standard output instead of a file")
	flag.Parse()

	dir := "."
	if flag.NArg() > 0 {
		dir = flag.Arg(0)
	}

	options := ormgen.Options{Dir: dir, All: *all, Package: *packageName}
	if *typeList != "" {
		for _, name := range strings.Split(*typeList, ",") {
			if trimmed := strings.TrimSpace(name); trimmed != "" {
				options.Only = append(options.Only, trimmed)
			}
		}
	}

	source, err := ormgen.Generate(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	if *toStdout {
		os.Stdout.Write(source)
		return
	}

	path := filepath.Join(dir, *out)
	if err := os.WriteFile(path, source, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "ormgen: wrote %s\n", path)
}
