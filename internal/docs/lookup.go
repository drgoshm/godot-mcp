package docs

import (
	"fmt"
	"sort"
	"strings"
)

// ProjectClass — класс проекта с class_name (из кеша глобальных классов).
type ProjectClass struct{ Name, Base, Path string }

// Lookup отвечает на запрос по имени класса и, возможно, члена.
// name может быть "Class.member". Если ничего не нашлось, в ответе
// объясняется почему: переименование из Godot 3, класс проекта или похожие имена.
func (ix *Index) Lookup(name, member string, inherited bool, project []ProjectClass) (text string, found bool) {
	name = strings.TrimSpace(name)
	if member == "" {
		if i := strings.LastIndex(name, "."); i > 0 && ix.Class(name[:i]) != nil {
			name, member = name[:i], name[i+1:]
		}
	}
	member = strings.TrimSuffix(strings.TrimSpace(member), "()")

	c := ix.Class(name)
	var note string
	if c == nil {
		if r, ok := classRenames[strings.ToLower(name)]; ok {
			if r.New == "" {
				return fmt.Sprintf("%s does not exist in Godot 4: %s.", name, r.Note), false
			}
			note = fmt.Sprintf("Note: %s is the Godot 3 name; in Godot 4 it is %s.", name, r.New)
			if r.Note != "" {
				note += " " + r.Note
			}
			c = ix.Class(r.New)
		}
	}
	if c == nil {
		return ix.notFoundClass(name, project), false
	}

	if member == "" {
		return prefix(note, ix.Overview(c, inherited)), true
	}
	if text := ix.Member(c, member); text != "" {
		return prefix(note, text), true
	}
	return prefix(note, ix.notFoundMember(c, member)), false
}

func prefix(note, text string) string {
	if note == "" {
		return text
	}
	return note + "\n\n" + text
}

func (ix *Index) notFoundClass(name string, project []ProjectClass) string {
	var b strings.Builder
	for _, p := range project {
		if strings.EqualFold(p.Name, name) {
			fmt.Fprintf(&b, "%s is a class of this project (class_name in %s), extending %s. Read that script for its API", p.Name, p.Path, p.Base)
			if ix.Class(p.Base) != nil {
				fmt.Fprintf(&b, "; for the inherited engine API look up %s", p.Base)
			}
			b.WriteString(".\n")
			return b.String()
		}
	}
	fmt.Fprintf(&b, "There is no class %q in Godot %s.\n", name, ix.Version)
	if hint, ok := memberRenames[strings.ToLower(name)]; ok {
		b.WriteString("Godot 3 → 4: " + hint + "\n")
	}
	if owners := ix.owners(name, 15); len(owners) > 0 {
		fmt.Fprintf(&b, "%q is a member of: %s. Look it up with name=<class> member=%s.\n", name, strings.Join(owners, ", "), name)
	}
	if s := similar(name, ix.names, 8); len(s) > 0 {
		b.WriteString("Similar classes: " + strings.Join(s, ", ") + "\n")
	}
	b.WriteString("Use search to find classes and members by part of the name.\n")
	return b.String()
}

func (ix *Index) notFoundMember(c *Class, member string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s has no member %q (searched %s and its base classes).\n", c.Name, member, c.Name)
	if hint, ok := memberRenames[strings.ToLower(member)]; ok {
		b.WriteString("Godot 3 → 4: " + hint + "\n")
	}
	var names []string
	for _, a := range ix.Chain(c) {
		for _, n := range memberNames(a) {
			names = append(names, strings.TrimPrefix(n, "signal "))
		}
	}
	if s := similar(member, names, 10); len(s) > 0 {
		b.WriteString("Similar members: " + strings.Join(s, ", ") + "\n")
	}
	if owners := ix.owners(member, 10); len(owners) > 0 {
		b.WriteString("Classes that do have it: " + strings.Join(owners, ", ") + "\n")
	}
	return b.String()
}

// owners — классы, у которых есть член с таким именем (собственный, не унаследованный).
func (ix *Index) owners(member string, limit int) []string {
	member = strings.TrimSuffix(member, "()")
	var out []string
	for _, n := range ix.names {
		c := ix.classes[strings.ToLower(n)]
		if hasMember(c, member) {
			out = append(out, c.Name)
			if len(out) == limit {
				break
			}
		}
	}
	return out
}

func hasMember(c *Class, name string) bool {
	for _, m := range c.Methods {
		if strings.EqualFold(m.Name, name) && !m.Constructor {
			return true
		}
	}
	for _, p := range c.Properties {
		if strings.EqualFold(p.Name, name) {
			return true
		}
	}
	for _, s := range c.Signals {
		if strings.EqualFold(s.Name, name) {
			return true
		}
	}
	return false
}

// ---- поиск ----

type hit struct {
	score int
	text  string
}

// Search ищет классы и члены по части имени. Точное совпадение выше
// префикса, префикс выше вхождения; классы выше членов.
func (ix *Index) Search(query string, limit int) string {
	q := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(query), "()"))
	if q == "" {
		return "search is empty"
	}
	score := func(name string) int {
		n := strings.ToLower(name)
		switch {
		case n == q:
			return 3
		case strings.HasPrefix(n, q):
			return 2
		case strings.Contains(n, q):
			return 1
		}
		return 0
	}
	var hits []hit
	for _, n := range ix.names {
		c := ix.classes[strings.ToLower(n)]
		if s := score(c.Name); s > 0 {
			hits = append(hits, hit{s*10 + 5, fmt.Sprintf("- %s — class — %s", c.Name, brief(c.Brief, briefLen))})
		}
		for _, m := range c.Methods {
			if s := score(m.Name); s > 0 && !m.Constructor {
				hits = append(hits, hit{s * 10, fmt.Sprintf("- %s.%s — %s", c.Name, m.Signature(), brief(m.Description, 100))})
			}
		}
		for _, p := range c.Properties {
			if s := score(p.Name); s > 0 {
				hits = append(hits, hit{s * 10, fmt.Sprintf("- %s.%s — property — %s", c.Name, p.Signature(), brief(p.Description, 100))})
			}
		}
		for _, sg := range c.Signals {
			if s := score(sg.Name); s > 0 {
				hits = append(hits, hit{s * 10, fmt.Sprintf("- %s.%s — signal — %s", c.Name, sg.Signature(), brief(sg.Description, 100))})
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })

	var b strings.Builder
	if r, ok := classRenames[q]; ok {
		if r.New != "" {
			fmt.Fprintf(&b, "Godot 3 → 4: %s is now %s. %s\n", query, r.New, r.Note)
		} else {
			fmt.Fprintf(&b, "Godot 3 → 4: %s %s\n", query, r.Note)
		}
	}
	if hint, ok := memberRenames[q]; ok {
		b.WriteString("Godot 3 → 4: " + hint + "\n")
	}
	if len(hits) == 0 {
		fmt.Fprintf(&b, "Nothing in Godot %s matches %q.\n", ix.Version, query)
		if s := similar(query, ix.names, 8); len(s) > 0 {
			b.WriteString("Similar classes: " + strings.Join(s, ", ") + "\n")
		}
		return b.String()
	}
	fmt.Fprintf(&b, "%d matches for %q in Godot %s", len(hits), query, ix.Version)
	if len(hits) > limit {
		fmt.Fprintf(&b, " (showing %d; refine the query)", limit)
		hits = hits[:limit]
	}
	b.WriteString(":\n")
	for _, h := range hits {
		b.WriteString(h.text + "\n")
	}
	return b.String()
}

// similar — имена, похожие на name: содержат его (или наоборот) или отличаются
// на пару правок. Для опечаток и полузабытых имён.
func similar(name string, names []string, limit int) []string {
	q := strings.ToLower(strings.TrimSuffix(name, "()"))
	type cand struct {
		name string
		dist int
	}
	var cands []cand
	seen := map[string]bool{}
	for _, n := range names {
		l := strings.ToLower(strings.TrimSuffix(n, "()"))
		if seen[l] || l == q {
			continue
		}
		d := levenshtein(q, l)
		if (len(q) >= 3 && (strings.Contains(l, q) || strings.Contains(q, l) && len(l) >= 4)) || d <= max(1, len(q)/4) {
			seen[l] = true
			cands = append(cands, cand{n, d})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].dist < cands[j].dist })
	var out []string
	for i := 0; i < len(cands) && i < limit; i++ {
		out = append(out, cands[i].name)
	}
	return out
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	cur := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(b)]
}
