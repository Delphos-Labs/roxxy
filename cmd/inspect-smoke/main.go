// inspect-smoke: ad-hoc tool to run an npm package URL through the
// inspect.NPMInspector. Not shipped; used to validate against real
// packages.
package main

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/Delphos-Labs/roxxy/internal/inspect"
	"github.com/Delphos-Labs/roxxy/internal/util"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: inspect-smoke <url> <name> <version>")
		os.Exit(2)
	}
	url, name, version := os.Args[1], os.Args[2], os.Args[3]
	resp, err := http.Get(url)
	if err != nil {
		panic(err)
	}
	defer util.DeferClose(resp.Body)
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		panic(err)
	}
	fmt.Printf("%s@%s — %d bytes\n", name, version, len(body))
	findings, err := inspect.NPMInspector{}.Inspect(body, name, version)
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
		return
	}
	if len(findings) == 0 {
		fmt.Println("  (no findings, clean)")
		return
	}
	for _, f := range findings {
		fmt.Printf("  %s\n", f)
	}
}
