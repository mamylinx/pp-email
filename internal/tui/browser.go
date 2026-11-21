package tui

import "charm.land/bubbles/v2/table"

type browser struct {
	table   table.Model
	indices []int
}

func newBrowser() browser { return browser{table: table.New(table.WithFocused(true))} }

func (b *browser) selected() (int, bool) {
	if cursor := b.table.Cursor(); cursor >= 0 && cursor < len(b.indices) {
		return b.indices[cursor], true
	}
	return 0, false
}

func (b *browser) setRows(rows []table.Row, indices []int) {
	previous, hadSelection := b.selected()
	b.indices = indices
	b.table.SetRows(rows)
	for cursor, index := range indices {
		if hadSelection && index == previous {
			b.table.SetCursor(cursor)
			return
		}
	}
	b.table.GotoTop()
}

// filterRows keeps the source indices so callers can map a row back to its
// original item.
func filterRows[T any](items []T, match func(T) bool, row func(T) table.Row) ([]table.Row, []int) {
	rows := make([]table.Row, 0, len(items))
	indices := make([]int, 0, len(items))
	for i, item := range items {
		if match(item) {
			rows = append(rows, row(item))
			indices = append(indices, i)
		}
	}
	return rows, indices
}
