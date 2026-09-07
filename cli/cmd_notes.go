package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// cmdRemember stores an explicit note (local, no gateway needed).
func cmdRemember(args []string) int {
	text := strings.Join(args, " ")
	line, err := notesAppend(text)
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	fmt.Printf("已记住 #%d: %s\n", line, strings.ReplaceAll(text, "\n", " "))
	return 0
}

// cmdNotes manages the notes store: list / find / forget.
func cmdNotes(args []string) int {
	if len(args) == 0 {
		return notesList()
	}
	switch args[0] {
	case "list":
		return notesList()
	case "find":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "gw: usage: gw notes find <query>")
			return 2
		}
		out, err := findNotes(strings.Join(args[1:], " "), 0)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gw:", err)
			return 1
		}
		fmt.Println(out)
		return 0
	case "forget":
		if len(args) < 2 {
			fmt.Fprintln(os.Stderr, "gw: usage: gw notes forget <line>")
			return 2
		}
		line, err := strconv.Atoi(args[1])
		if err != nil || line < 1 {
			fmt.Fprintln(os.Stderr, "gw: 需要行号(正整数)")
			return 2
		}
		removed, err := notesForget(line)
		if err != nil {
			fmt.Fprintln(os.Stderr, "gw:", err)
			return 1
		}
		fmt.Printf("已忘记 #%d: %s\n", line, strings.ReplaceAll(removed.Text, "\n", " "))
		return 0
	case "count":
		items, err := readNotes()
		if err != nil {
			if os.IsNotExist(err) {
				fmt.Println(0)
				return 0
			}
			fmt.Fprintln(os.Stderr, "gw:", err)
			return 1
		}
		fmt.Println(len(items))
		return 0
	default:
		fmt.Fprintf(os.Stderr, "gw: unknown notes subcommand %q\n", args[0])
		return 2
	}
}

func notesList() int {
	out, err := listNotes()
	if err != nil {
		fmt.Fprintln(os.Stderr, "gw:", err)
		return 1
	}
	fmt.Println(out)
	return 0
}
