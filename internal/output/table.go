package output

import (
	"fmt"
	"io"
	"strings"
)

func PrintTable(w io.Writer, headers []string, rows [][]string) {
	if len(headers) == 0 {
		return
	}
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}

	normalizedRows := make([][]string, 0, len(rows))
	for _, row := range rows {
		normalized := normalizeRow(row, len(headers))
		normalizedRows = append(normalizedRows, normalized)
		for i, c := range normalized {
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
	for _, row := range normalizedRows {
		printRow(w, widths, row)
	}
}

func normalizeRow(row []string, cols int) []string {
	if len(row) == cols {
		return row
	}
	normalized := make([]string, cols)
	copy(normalized, row)
	return normalized
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
