// Command scode is a lean Go coding agent built on pi's architecture.
// M0: message model only; CLI modes arrive with M5.
package main

import (
	"fmt"
	"os"

	"scode/internal/llm"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("scode dev (M0: message model)")
		return
	}
	// Smoke: prove the message model round-trips. Real modes land in M5.
	tr, err := llm.NormalizeContext(llm.Context{
		SystemPrompt: "scode",
		Tools:        []llm.Tool{{Name: "read", Description: "read a file"}},
		Messages:     []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{llm.TextBlock("hello")}, TS: 1}},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "fatal:", err)
		os.Exit(1)
	}
	b, _ := llm.CanonicalBytes(tr.Messages())
	os.Stdout.Write(b)
}
