package tui

import (
	"fmt"
	"strings"

	"charm.land/lipgloss/v2"
)

// launcher_view.go renders the launcher: the network list, the add/edit form, and
// the delete-confirm prompt. It reuses defaultTheme so the launcher matches the
// chat UI.

// renderLauncher composes the full launcher frame for the current mode.
func renderLauncher(m launcherModel) string {
	t := defaultTheme

	title := t.statusKey.Render("lurk") + t.dim.Render(" — choose a network")
	var body, hint string

	switch m.mode {
	case modeForm:
		title = t.statusKey.Render(m.form.title)
		body = renderForm(m.form, t)
		hint = "tab/↑↓ move · space toggle · enter save · esc cancel"
	case modeConfirm:
		body = renderNetworkRows(m, t)
		name := ""
		if m.sel < len(m.store.Networks) {
			name = m.store.Networks[m.sel].Name
		}
		body += "\n\n" + t.notice.Render(fmt.Sprintf("delete %q?  (y/n)", name))
		hint = "y delete · n cancel"
	default:
		body = renderNetworkRows(m, t)
		hint = "↑↓ select · enter connect · a add · e edit · d delete · q quit"
	}

	lines := []string{title, "", body, ""}
	if m.status != "" {
		lines = append(lines, t.info.Render(m.status), "")
	}
	lines = append(lines, t.dim.Render(hint))
	return lipgloss.NewStyle().Padding(1, 2).Render(strings.Join(lines, "\n"))
}

// renderNetworkRows renders the list of saved networks plus the "add" row, with
// the selected row highlighted.
func renderNetworkRows(m launcherModel, t theme) string {
	var rows []string
	if len(m.store.Networks) == 0 {
		rows = append(rows, t.dim.Render("no saved networks yet"))
	}
	for i, n := range m.store.Networks {
		meta := n.Addr
		var flags []string
		if n.TLS {
			flags = append(flags, "TLS")
		}
		if n.SASL.Mechanism != "" {
			flags = append(flags, "SASL")
		}
		if len(flags) > 0 {
			meta += "  " + strings.Join(flags, " ")
		}
		rows = append(rows, networkRow(t, i == m.sel, n.Name, meta))
	}
	rows = append(rows, networkRow(t, m.sel >= len(m.store.Networks), "＋ Add network", ""))
	return strings.Join(rows, "\n")
}

// networkRow renders one row of the network list.
func networkRow(t theme, selected bool, name, meta string) string {
	cursor := "  "
	nameStyled := t.sidebarItem.Render(name)
	if selected {
		cursor = t.statusKey.Render("▸ ")
		nameStyled = t.sidebarActive.Render(name)
	}
	if meta == "" {
		return cursor + nameStyled
	}
	return cursor + nameStyled + "  " + t.dim.Render(meta)
}

// renderForm renders the add/edit form's fields, marking the focused one.
func renderForm(f *netForm, t theme) string {
	rows := make([]string, 0, len(f.fields)+2)
	for i, fld := range f.fields {
		marker := "  "
		labelStyle := t.dim
		if i == f.idx {
			marker = t.statusKey.Render("▸ ")
			labelStyle = t.statusKey
		}
		label := labelStyle.Render(fmt.Sprintf("%-12s", fld.label))

		var val string
		switch fld.kind {
		case kindBool:
			val = boolBox(fld.on)
		case kindMech:
			val = "‹ " + mechLabel(fld.mech) + " ›"
		default:
			val = fld.input.View()
		}
		rows = append(rows, marker+label+" "+val)
	}
	out := strings.Join(rows, "\n")
	if f.err != "" {
		out += "\n\n" + t.notice.Render("! "+f.err)
	}
	return out
}

// boolBox renders a toggle as "[x]" or "[ ]".
func boolBox(on bool) string {
	if on {
		return "[x]"
	}
	return "[ ]"
}

// mechLabel renders the SASL mechanism at cycle index i ("none" for disabled).
func mechLabel(i int) string {
	if saslMechs[i] == "" {
		return "none"
	}
	return saslMechs[i]
}
