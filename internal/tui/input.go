package tui

import (
	"slices"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// textField keeps a single editable line for searches and export paths.
type textField struct {
	value  []rune
	cursor int
	width  int
}

func (f *textField) SetValue(value string) {
	f.value = []rune(value)
	f.cursor = len(f.value)
}

func (f textField) Value() string { return string(f.value) }

func (f *textField) Update(msg tea.Msg) {
	switch msg := msg.(type) {
	case tea.PasteMsg:
		f.insert(msg.Content)
	case tea.KeyPressMsg:
		switch msg.String() {
		case "left":
			f.cursor = max(0, f.cursor-1)
		case "right":
			f.cursor = min(len(f.value), f.cursor+1)
		case "home", "ctrl+a":
			f.cursor = 0
		case "end", "ctrl+e":
			f.cursor = len(f.value)
		case "backspace":
			if f.cursor > 0 {
				f.value = slices.Delete(f.value, f.cursor-1, f.cursor)
				f.cursor--
			}
		case "delete":
			if f.cursor < len(f.value) {
				f.value = slices.Delete(f.value, f.cursor, f.cursor+1)
			}
		case "ctrl+u":
			f.SetValue("")
		default:
			if msg.Text != "" {
				f.insert(msg.Text)
			} else if msg.Mod == 0 && unicode.IsPrint(msg.Code) {
				f.insert(string(msg.Code))
			}
		}
	}
}

func (f *textField) insert(text string) {
	visible := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		return r
	}, text)
	runes := []rune(visible)
	f.value = slices.Insert(f.value, f.cursor, runes...)
	f.cursor += len(runes)
}

func (f textField) View(placeholder string) string {
	if len(f.value) == 0 {
		return "▏" + placeholder
	}
	before := string(f.value[:f.cursor])
	after := string(f.value[f.cursor:])
	if trim := ansi.StringWidth(before) - max(1, f.width-6); trim > 0 {
		before = ansi.TruncateLeft(before, trim, "…")
	}
	return ansi.Truncate(before+"▏"+after, f.width, "…")
}
