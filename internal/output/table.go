package output

import (
	"fmt"
	"io"
	"strings"
)

func PrintTable(w io.Writer, headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, row := range rows {
		for i, c := range row {
			if len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	printRow(w, widths, headers)
	sep := make([]string, len(headers))
	for i := range headers {
		sep[i] = strings.Repeat("-", widths[i])
	}
	printRow(w, widths, sep)
	for _, row := range rows {
		printRow(w, widths, row)
	}
}

func printRow(w io.Writer, widths []int, cells []string) {
	for i, c := range cells {
		fmt.Fprintf(w, "%-*s", widths[i], c)
		if i < len(cells)-1 {
			fmt.Fprint(w, "  ")
		}
	}
	fmt.Fprintln(w)
}
