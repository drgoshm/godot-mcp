package docs

import (
	"fmt"
	"regexp"
	"strings"
)

// ---- BBCode из описаний Godot -> Markdown ----

var (
	// [codeblocks][gdscript]...[/gdscript][csharp]...[/csharp][/codeblocks]: оставляем GDScript.
	codeblocksRe = regexp.MustCompile(`(?s)\[codeblocks\](.*?)\[/codeblocks\]`)
	gdscriptRe   = regexp.MustCompile(`(?s)\[gdscript[^\]]*\](.*?)\[/gdscript\]`)
	codeblockRe  = regexp.MustCompile(`(?s)\[codeblock[^\]]*\](.*?)\[/codeblock\]`)
	codeRe       = regexp.MustCompile(`(?s)\[(?:code|kbd)\](.*?)\[/(?:code|kbd)\]`)
	refRe        = regexp.MustCompile(`\[(method|member|signal|constant|enum|annotation|constructor|operator|theme_item|param) ([^\]]+)\]`)
	urlRe        = regexp.MustCompile(`\[url=([^\]]+)\](.*?)\[/url\]`)
	classRefRe   = regexp.MustCompile(`\[([A-Z@][A-Za-z0-9_]*)\]`)
	styleRe      = regexp.MustCompile(`\[/?(?:b|i|u|s|center|lb|rb|font[^\]]*|color[^\]]*)\]`)
)

// markdown переводит BBCode справки Godot в Markdown.
func markdown(s string) string {
	s = codeblocksRe.ReplaceAllStringFunc(s, func(block string) string {
		if m := gdscriptRe.FindStringSubmatch(block); m != nil {
			return fence(m[1])
		}
		return ""
	})
	s = codeblockRe.ReplaceAllStringFunc(s, func(block string) string {
		return fence(codeblockRe.FindStringSubmatch(block)[1])
	})
	s = codeRe.ReplaceAllString(s, "`$1`")
	s = refRe.ReplaceAllStringFunc(s, func(r string) string {
		m := refRe.FindStringSubmatch(r)
		switch m[1] {
		case "method", "constructor":
			return "`" + m[2] + "()`"
		case "annotation":
			return "`" + m[2] + "`"
		}
		return "`" + m[2] + "`"
	})
	s = urlRe.ReplaceAllString(s, "$2 ($1)")
	s = strings.NewReplacer("[br]", "\n", "[lb]", "[", "[rb]", "]").Replace(s)
	s = styleRe.ReplaceAllString(s, "")
	s = classRefRe.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

func fence(code string) string {
	// Перед закрывающим тегом стоят табы вложенности — это не строка кода.
	lines := strings.Split(strings.TrimLeft(strings.TrimRight(code, " \t\n"), "\n"), "\n")
	// В XML код отбит табами по вложенности тегов — убираем общий отступ.
	indent := -1
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			continue
		}
		n := len(l) - len(strings.TrimLeft(l, "\t"))
		if indent < 0 || n < indent {
			indent = n
		}
	}
	for i, l := range lines {
		if len(l) >= indent && indent > 0 {
			lines[i] = l[indent:]
		}
	}
	return "\n```gdscript\n" + strings.Join(lines, "\n") + "\n```\n"
}

// brief — первое предложение без разметки, не длиннее n символов.
func brief(s string, n int) string {
	s = markdown(s)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[:i]
	}
	s = strings.Join(strings.Fields(s), " ")
	if i := strings.Index(s, ". "); i > 0 && i < n {
		s = s[:i+1]
	}
	if len(s) > n {
		cut := strings.LastIndex(s[:n], " ")
		if cut < n/2 {
			cut = n
		}
		s = s[:cut] + "…"
	}
	return s
}

// ---- сигнатуры ----

func (m Method) Signature() string {
	var b strings.Builder
	if m.Static {
		b.WriteString("static ")
	}
	b.WriteString(m.Name)
	b.WriteString("(")
	for i, a := range m.Args {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(a.String())
	}
	if m.Vararg {
		if len(m.Args) > 0 {
			b.WriteString(", ")
		}
		b.WriteString("...")
	}
	b.WriteString(")")
	if !m.Constructor && m.Return != "" {
		b.WriteString(" -> " + m.Return)
	}
	var q []string
	if m.Const {
		q = append(q, "const")
	}
	if m.Virtual {
		q = append(q, "virtual")
	}
	if len(q) > 0 {
		b.WriteString(" [" + strings.Join(q, ", ") + "]")
	}
	return b.String()
}

func (a Arg) String() string {
	s := a.Name
	if a.Type != "" {
		s += ": " + a.Type
	}
	if a.Default != "" {
		s += " = " + a.Default
	}
	return s
}

func (p Property) Signature() string {
	t := p.Type
	if p.Enum != "" {
		t = p.Enum
	}
	s := p.Name + ": " + t
	if p.Default != "" {
		s += " = " + p.Default
	}
	return s
}

func (s Signal) Signature() string {
	args := make([]string, len(s.Args))
	for i, a := range s.Args {
		args[i] = a.String()
	}
	return s.Name + "(" + strings.Join(args, ", ") + ")"
}

// ---- обзор класса ----

const (
	briefLen       = 140
	maxDescription = 2500
)

// Overview — обзор класса: предки, описание, собственные члены с краткими
// описаниями. inherited=true добавляет имена унаследованных членов.
func (ix *Index) Overview(c *Class, inherited bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s", c.Name)
	switch {
	case c.Singleton:
		b.WriteString(" (singleton: use it as " + c.Name + ".method())")
	case c.Kind == "builtin":
		b.WriteString(" (built-in type)")
	}
	fmt.Fprintf(&b, "\nGodot %s\n", ix.Version)
	if chain := ix.Chain(c); len(chain) > 1 {
		names := make([]string, 0, len(chain)-1)
		for _, a := range chain[1:] {
			names = append(names, a.Name)
		}
		b.WriteString("Inherits: " + strings.Join(names, " < ") + "\n")
	}
	if c.Brief != "" {
		b.WriteString("\n" + markdown(c.Brief) + "\n")
	}
	if d := markdown(c.Description); d != "" {
		if len(d) > maxDescription {
			d = d[:maxDescription] + "…\n(description truncated)"
		}
		b.WriteString("\n" + d + "\n")
	}

	if len(c.Properties) > 0 {
		b.WriteString("\n## Properties\n")
		for _, p := range c.Properties {
			line(&b, p.Signature(), p.Description)
		}
	}
	// Сеттеры и геттеры свойств дублируют список свойств — в обзоре их не показываем.
	accessors := map[string]bool{}
	for _, p := range c.Properties {
		accessors[p.Setter], accessors[p.Getter] = true, true
	}
	var ctors, methods, virtuals []Method
	hidden := 0
	for _, m := range c.Methods {
		switch {
		case accessors[m.Name] && m.Name != "":
			hidden++
		case m.Constructor:
			ctors = append(ctors, m)
		case m.Virtual:
			virtuals = append(virtuals, m)
		default:
			methods = append(methods, m)
		}
	}
	section := func(title string, ms []Method) {
		if len(ms) == 0 {
			return
		}
		b.WriteString("\n## " + title + "\n")
		for _, m := range ms {
			line(&b, m.Signature(), m.Description)
		}
	}
	section("Constructors", ctors)
	section("Methods", methods)
	if hidden > 0 {
		fmt.Fprintf(&b, "(%d property setters/getters such as set_x()/get_x() are omitted; use the properties)\n", hidden)
	}
	section("Virtual methods (override in your script)", virtuals)
	if len(c.Signals) > 0 {
		b.WriteString("\n## Signals\n")
		for _, s := range c.Signals {
			line(&b, s.Signature(), s.Description)
		}
	}
	if len(c.Enums) > 0 {
		b.WriteString("\n## Enums\n")
		for _, e := range c.Enums {
			vals := make([]string, len(e.Values))
			for i, v := range e.Values {
				vals[i] = v.Name + " = " + v.Value
			}
			kind := "enum"
			if e.Bitfield {
				kind = "flags"
			}
			fmt.Fprintf(&b, "- %s %s: %s\n", kind, e.Name, strings.Join(vals, ", "))
		}
	}
	if len(c.Constants) > 0 {
		b.WriteString("\n## Constants\n")
		for _, k := range c.Constants {
			line(&b, k.Name+" = "+k.Value, k.Description)
		}
	}

	if inherited {
		for _, a := range ix.Chain(c)[1:] {
			names := memberNames(a)
			if len(names) > 0 {
				fmt.Fprintf(&b, "\n## Inherited from %s\n%s\n", a.Name, strings.Join(names, ", "))
			}
		}
	} else if chain := ix.Chain(c); len(chain) > 1 {
		b.WriteString("\nInherited members are not listed; pass inherited=true, or member=<name> to look one up through the chain.\n")
	}
	return b.String()
}

func line(b *strings.Builder, sig, desc string) {
	if d := brief(desc, briefLen); d != "" {
		fmt.Fprintf(b, "- %s — %s\n", sig, d)
	} else {
		fmt.Fprintf(b, "- %s\n", sig)
	}
}

func memberNames(c *Class) []string {
	var names []string
	for _, p := range c.Properties {
		names = append(names, p.Name)
	}
	for _, m := range c.Methods {
		if !m.Constructor {
			names = append(names, m.Name+"()")
		}
	}
	for _, s := range c.Signals {
		names = append(names, "signal "+s.Name)
	}
	return names
}

// ---- один член ----

// Member — полное описание члена класса; ищется вверх по цепочке предков.
// Возвращает "" если такого члена нет.
func (ix *Index) Member(c *Class, name string) string {
	name = strings.TrimSuffix(strings.TrimSpace(name), "()")
	for _, owner := range ix.Chain(c) {
		var b strings.Builder
		header := func(kind, sig string) {
			fmt.Fprintf(&b, "# %s.%s (%s)\nGodot %s\n", owner.Name, name, kind, ix.Version)
			if owner != c {
				fmt.Fprintf(&b, "Inherited by %s from %s.\n", c.Name, owner.Name)
			}
			b.WriteString("\n```gdscript\n" + sig + "\n```\n")
		}
		for _, m := range owner.Methods {
			if strings.EqualFold(m.Name, name) && !m.Constructor {
				kind := "method"
				if m.Virtual {
					kind = "virtual method: override it in your script"
				}
				header(kind, m.Signature())
				body(&b, m.Description)
				return b.String()
			}
		}
		for _, p := range owner.Properties {
			if strings.EqualFold(p.Name, name) {
				header("property", p.Signature())
				if p.Enum != "" {
					if e := ix.enumValues(p.Enum); e != "" {
						b.WriteString("\nValues: " + e + "\n")
					}
				}
				body(&b, p.Description)
				return b.String()
			}
		}
		for _, s := range owner.Signals {
			if strings.EqualFold(s.Name, name) {
				header("signal", "signal "+s.Signature())
				body(&b, s.Description)
				return b.String()
			}
		}
		for _, e := range owner.Enums {
			if strings.EqualFold(e.Name, name) {
				header("enum", "enum "+e.Name)
				for _, v := range e.Values {
					line(&b, v.Name+" = "+v.Value, v.Description)
				}
				return b.String()
			}
			for _, v := range e.Values {
				if strings.EqualFold(v.Name, name) {
					header("constant of enum "+e.Name, v.Name+" = "+v.Value)
					body(&b, v.Description)
					return b.String()
				}
			}
		}
		for _, k := range owner.Constants {
			if strings.EqualFold(k.Name, name) {
				header("constant", k.Name+" = "+k.Value)
				body(&b, k.Description)
				return b.String()
			}
		}
		// Конструкторы встроенных типов: "Vector2.Vector2" или "Color.Color".
		if strings.EqualFold(owner.Name, name) {
			header("constructors", "")
			for _, m := range owner.Methods {
				if m.Constructor {
					line(&b, m.Signature(), m.Description)
				}
			}
			return b.String()
		}
	}
	return ""
}

func body(b *strings.Builder, desc string) {
	if d := markdown(desc); d != "" {
		b.WriteString("\n" + d + "\n")
	} else {
		b.WriteString("\n(no description in the engine's reference)\n")
	}
}

// enumValues: "CharacterBody2D.MotionMode" -> "MOTION_MODE_GROUNDED = 0, ...".
func (ix *Index) enumValues(ref string) string {
	cls, name := "@GlobalScope", ref
	if i := strings.LastIndex(ref, "."); i > 0 {
		cls, name = ref[:i], ref[i+1:]
	}
	c := ix.Class(cls)
	if c == nil {
		return ""
	}
	for _, e := range c.Enums {
		if e.Name == name {
			vals := make([]string, len(e.Values))
			for i, v := range e.Values {
				vals[i] = v.Name + " = " + v.Value
			}
			return strings.Join(vals, ", ")
		}
	}
	return ""
}
