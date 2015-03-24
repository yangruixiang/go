// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// This program generates Go code that applies rewrite rules to a Value.
// The generated code implements a function of type func (v *Value) bool
// which returns true iff if did something.
// Ideas stolen from Swift: http://www.hpl.hp.com/techreports/Compaq-DEC/WRL-2000-2.html

// Run with something like "go run rulegen.go lower_amd64.rules lowerAmd64 lowerAmd64.go"

package main

import (
	"bufio"
	"bytes"
	"fmt"
	"go/format"
	"io"
	"log"
	"os"
	"sort"
	"strings"
)

// rule syntax:
//  sexpr [&& extra conditions] -> sexpr
//
// sexpr are s-expressions (lisp-like parenthesized groupings)
// sexpr ::= (opcode sexpr*)
//         | variable
//         | [aux]
//         | <type>
//         | {code}
//
// aux      ::= variable | {code}
// type     ::= variable | {code}
// variable ::= some token
// opcode   ::= one of the opcodes from ../op.go (without the Op prefix)

// extra conditions is just a chunk of Go that evaluates to a boolean.  It may use
// variables declared in the matching sexpr.  The variable "v" is predefined to be
// the value matched by the entire rule.

// If multiple rules match, the first one in file order is selected.

func main() {
	if len(os.Args) < 3 || len(os.Args) > 4 {
		fmt.Printf("usage: go run rulegen.go <rule file> <function name> [<output file>]")
		os.Exit(1)
	}
	rulefile := os.Args[1]
	rulefn := os.Args[2]

	// Open input file.
	text, err := os.Open(rulefile)
	if err != nil {
		log.Fatalf("can't read rule file: %v", err)
	}

	// oprules contains a list of rules for each opcode
	oprules := map[string][]string{}

	// read rule file
	scanner := bufio.NewScanner(text)
	for scanner.Scan() {
		line := scanner.Text()
		if i := strings.Index(line, "//"); i >= 0 {
			// Remove comments.  Note that this isn't string safe, so
			// it will truncate lines with // inside strings.  Oh well.
			line = line[:i]
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		op := strings.Split(line, " ")[0][1:]
		oprules[op] = append(oprules[op], line)
	}
	if err := scanner.Err(); err != nil {
		log.Fatalf("scanner failed: %v\n", err)
	}

	// Start output buffer, write header.
	w := new(bytes.Buffer)
	fmt.Fprintf(w, "// autogenerated from %s: do not edit!\n", rulefile)
	fmt.Fprintf(w, "// generated with: go run rulegen/rulegen.go %s\n", strings.Join(os.Args[1:], " "))
	fmt.Fprintln(w, "package ssa")
	fmt.Fprintf(w, "func %s(v *Value) bool {\n", rulefn)

	// generate code for each rule
	fmt.Fprintf(w, "switch v.Op {\n")
	var ops []string
	for op := range oprules {
		ops = append(ops, op)
	}
	sort.Strings(ops)
	rulenum := 0
	for _, op := range ops {
		fmt.Fprintf(w, "case Op%s:\n", op)
		for _, rule := range oprules[op] {
			// split at ->
			s := strings.Split(rule, "->")
			if len(s) != 2 {
				log.Fatalf("no arrow in rule %s", rule)
			}
			lhs := strings.Trim(s[0], " \t")
			result := strings.Trim(s[1], " \t\n")

			// split match into matching part and additional condition
			match := lhs
			cond := ""
			if i := strings.Index(match, "&&"); i >= 0 {
				cond = strings.Trim(match[i+2:], " \t")
				match = strings.Trim(match[:i], " \t")
			}

			fmt.Fprintf(w, "// match: %s\n", match)
			fmt.Fprintf(w, "// cond: %s\n", cond)
			fmt.Fprintf(w, "// result: %s\n", result)

			fail := fmt.Sprintf("{\ngoto end%d\n}\n", rulenum)

			fmt.Fprintf(w, "{\n")
			genMatch(w, match, fail)

			if cond != "" {
				fmt.Fprintf(w, "if !(%s) %s", cond, fail)
			}

			genResult(w, result)
			fmt.Fprintf(w, "return true\n")

			fmt.Fprintf(w, "}\n")
			fmt.Fprintf(w, "end%d:;\n", rulenum)
			rulenum++
		}
	}
	fmt.Fprintf(w, "}\n")
	fmt.Fprintf(w, "return false\n")
	fmt.Fprintf(w, "}\n")

	// gofmt result
	b := w.Bytes()
	b, err = format.Source(b)
	if err != nil {
		panic(err)
	}

	// Write to a file if given, otherwise stdout.
	var out io.WriteCloser
	if len(os.Args) >= 4 {
		outfile := os.Args[3]
		out, err = os.Create(outfile)
		if err != nil {
			log.Fatalf("can't open output file %s: %v\n", outfile, err)
		}
	} else {
		out = os.Stdout
	}
	if _, err = out.Write(b); err != nil {
		log.Fatalf("can't write output: %v\n", err)
	}
	if err = out.Close(); err != nil {
		log.Fatalf("can't close output: %v\n", err)
	}
}

func genMatch(w io.Writer, match, fail string) {
	genMatch0(w, match, "v", fail, map[string]string{}, true)
}

func genMatch0(w io.Writer, match, v, fail string, m map[string]string, top bool) {
	if match[0] != '(' {
		if x, ok := m[match]; ok {
			// variable already has a definition.  Check whether
			// the old definition and the new definition match.
			// For example, (add x x).  Equality is just pointer equality
			// on Values (so cse is important to do before lowering).
			fmt.Fprintf(w, "if %s != %s %s", v, x, fail)
			return
		}
		// remember that this variable references the given value
		m[match] = v
		fmt.Fprintf(w, "%s := %s\n", match, v)
		return
	}

	// split body up into regions.  Split by spaces/tabs, except those
	// contained in () or {}.
	s := split(match[1 : len(match)-1])

	// check op
	if !top {
		fmt.Fprintf(w, "if %s.Op != Op%s %s", v, s[0], fail)
	}

	// check type/aux/args
	argnum := 0
	for _, a := range s[1:] {
		if a[0] == '<' {
			// type restriction
			t := a[1 : len(a)-1]
			if t[0] == '{' {
				// code.  We must match the results of this code.
				fmt.Fprintf(w, "if %s.Type != %s %s", v, t[1:len(t)-1], fail)
			} else {
				// variable
				if u, ok := m[t]; ok {
					// must match previous variable
					fmt.Fprintf(w, "if %s.Type != %s %s", v, u, fail)
				} else {
					m[t] = v + ".Type"
					fmt.Fprintf(w, "%s := %s.Type\n", t, v)
				}
			}
		} else if a[0] == '[' {
			// aux restriction
			x := a[1 : len(a)-1]
			if x[0] == '{' {
				// code
				fmt.Fprintf(w, "if %s.Aux != %s %s", v, x[1:len(x)-1], fail)
			} else {
				// variable
				if y, ok := m[x]; ok {
					fmt.Fprintf(w, "if %s.Aux != %s %s", v, y, fail)
				} else {
					m[x] = v + ".Aux"
					fmt.Fprintf(w, "%s := %s.Aux\n", x, v)
				}
			}
		} else if a[0] == '{' {
			fmt.Fprintf(w, "if %s.Args[%d] != %s %s", v, argnum, a[1:len(a)-1], fail)
			argnum++
		} else {
			// variable or sexpr
			genMatch0(w, a, fmt.Sprintf("%s.Args[%d]", v, argnum), fail, m, false)
			argnum++
		}
	}
}

func genResult(w io.Writer, result string) {
	genResult0(w, result, new(int), true)
}
func genResult0(w io.Writer, result string, alloc *int, top bool) string {
	if result[0] != '(' {
		// variable
		return result
	}

	s := split(result[1 : len(result)-1])
	var v string
	var needsType bool
	if top {
		v = "v"
		fmt.Fprintf(w, "v.Op = Op%s\n", s[0])
		fmt.Fprintf(w, "v.Aux = nil\n")
		fmt.Fprintf(w, "v.Args = v.argstorage[:0]\n")
	} else {
		v = fmt.Sprintf("v%d", *alloc)
		*alloc++
		fmt.Fprintf(w, "%s := v.Block.NewValue(Op%s, TypeInvalid, nil)\n", v, s[0])
		needsType = true
	}
	for _, a := range s[1:] {
		if a[0] == '<' {
			// type restriction
			t := a[1 : len(a)-1]
			if t[0] == '{' {
				t = t[1 : len(t)-1]
			}
			fmt.Fprintf(w, "%s.Type = %s\n", v, t)
			needsType = false
		} else if a[0] == '[' {
			// aux restriction
			x := a[1 : len(a)-1]
			if x[0] == '{' {
				x = x[1 : len(x)-1]
			}
			fmt.Fprintf(w, "%s.Aux = %s\n", v, x)
		} else if a[0] == '{' {
			fmt.Fprintf(w, "%s.AddArg(%s)\n", v, a[1:len(a)-1])
		} else {
			// regular argument (sexpr or variable)
			x := genResult0(w, a, alloc, false)
			fmt.Fprintf(w, "%s.AddArg(%s)\n", v, x)
		}
	}
	if needsType {
		fmt.Fprintf(w, "%s.SetType()\n", v)
	}
	return v
}

func split(s string) []string {
	var r []string

outer:
	for s != "" {
		d := 0         // depth of ({[<
		nonsp := false // found a non-space char so far
		for i := 0; i < len(s); i++ {
			switch s[i] {
			case '(', '{', '[', '<':
				d++
			case ')', '}', ']', '>':
				d--
			case ' ', '\t':
				if d == 0 && nonsp {
					r = append(r, strings.TrimSpace(s[:i]))
					s = s[i:]
					continue outer
				}
			default:
				nonsp = true
			}
		}
		if d != 0 {
			panic("imbalanced expression: " + s)
		}
		if nonsp {
			r = append(r, strings.TrimSpace(s))
		}
		break
	}
	return r
}