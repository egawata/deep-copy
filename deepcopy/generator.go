package deepcopy

import (
	"bytes"
	"errors"
	"fmt"
	"go/format"
	"go/types"
	"io"
	"log"
	"os"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/tools/go/packages"
)

type SkipLists []map[string]struct{}

func (l SkipLists) Get(i int) (s skips) {
	if i < len(l) {
		return l[i]
	}

	return s
}

type Generator struct {
	isPtrRecv  bool
	maxDepth   int
	methodName string
	skipLists  SkipLists

	imports       map[string]string
	fns           [][]byte
	receiverNames map[string]string
}

func NewGenerator(
	isPtrRecv bool, methodName string, skipLists SkipLists, maxDepth int,
) Generator {
	return Generator{
		isPtrRecv:  isPtrRecv,
		methodName: methodName,
		maxDepth:   maxDepth,
		skipLists:  skipLists,

		imports: map[string]string{},
		fns:     [][]byte{},
	}
}

type object interface {
	types.Type
	Obj() *types.TypeName
}

type pointer interface {
	Elem() types.Type
}

type methoder interface {
	types.Type
	Method(i int) *types.Func
	NumMethods() int
}

type skips map[string]struct{}

func (s skips) Contains(sel string) bool {
	if _, ok := s[sel]; ok {
		return ok
	}

	return false
}

func (g Generator) Generate(w io.Writer, types []string, p *packages.Package) error {
	objs := make([]object, len(types))
	for i, kind := range types {
		obj, err := locateType(kind, p)
		if err != nil {
			return fmt.Errorf("locating type %q in %q: %v", kind, p.Name, err)
		}

		objs[i] = obj
	}

	var err error
	g.receiverNames, err = getReceiverNames(p)
	if err != nil {
		return fmt.Errorf("getting receiver names: %v", err)
	}
	fmt.Printf("receivers = %#v\n", g.receiverNames)

	for i, obj := range objs {
		fn, err := g.generateFunc(p, obj, g.skipLists.Get(i), objs)
		if err != nil {
			return fmt.Errorf("generating method: %v", err)
		}

		g.fns = append(g.fns, fn)
	}

	err = g.generateFile(w, p)
	if err != nil {
		return fmt.Errorf("generating file content: %v", err)
	}

	return nil
}

func (g Generator) generateFunc(p *packages.Package, obj object, skips skips, generating []object) ([]byte, error) {
	var buf bytes.Buffer

	var ptr string
	if g.isPtrRecv {
		ptr = "*"
	}
	kind := obj.Obj().Name()

	source := "o"
	if g.receiverNames != nil && g.receiverNames[kind] != "" {
		fmt.Printf("receiver name for %s is %s\n", kind, g.receiverNames[kind])
		source = g.receiverNames[kind]
	}
	fmt.Fprintf(&buf, `// %s generates a deep copy of %s%s
func (%s %s%s) %s() %s%s {
	var cp %s = %s%s
`, g.methodName, ptr, kind, source, ptr, kind, g.methodName, ptr, kind, kind, ptr, source)

	g.walkType(source, "cp", p.Name, obj, &buf, skips, generating, 0)

	if g.isPtrRecv {
		buf.WriteString("return &cp\n}")
	} else {
		buf.WriteString("return cp\n}")
	}

	return buf.Bytes(), nil
}

func (g Generator) generateFile(w io.Writer, p *packages.Package) error {
	var file bytes.Buffer

	fmt.Fprintf(&file, "// Code generated by %s; DO NOT EDIT.\n\npackage %s\n\n", strings.Join(os.Args, " "), p.Name)

	if len(g.imports) > 0 {
		file.WriteString("import (\n")
		for name, path := range g.imports {
			if strings.HasSuffix(path, name) {
				fmt.Fprintf(&file, "%q\n", path)
			} else {
				fmt.Fprintf(&file, "%s %q\n", name, path)
			}
		}
		file.WriteString(")\n")
	}

	for _, fn := range g.fns {
		file.Write(fn)
		file.WriteString("\n\n")
	}

	b, err := format.Source(file.Bytes())
	if err != nil {
		return fmt.Errorf("error formatting source: %w\nsource:\n%s", err, file.String())
	}

	_, err = w.Write(b)
	return err
}

func (g Generator) walkType(source, sink, x string, m types.Type, w io.Writer, skips skips, generating []object, depth int) {
	initial := depth == 0
	if m == nil {
		return
	}

	if g.maxDepth > 0 {
		if depth >= g.maxDepth {
			p := strings.Split(sink, ".")
			stoppedAt := strings.TrimSuffix(fmt.Sprintf("%s.%s", generating[0], strings.Join(p[1:len(p)-1], ".")), ".")
			log.Printf("WARNING: reached max depth %d. stop recursion at %s", depth, stoppedAt)
			return
		}
	}

	var needExported bool
	switch v := m.(type) {
	case *types.Named:
		if v.Obj().Pkg() != nil && v.Obj().Pkg().Name() != x {
			needExported = true
		}
	}

	if v, ok := m.(methoder); ok && !initial && g.reuseDeepCopy(source, sink, v, false, generating, w) {
		return
	}

	depth++
	under := m.Underlying()
	switch v := under.(type) {
	case *types.Struct:
		for i := 0; i < v.NumFields(); i++ {
			field := v.Field(i)
			if needExported && !field.Exported() {
				continue
			}
			fname := field.Name()
			sel := sink + "." + fname
			sel = sel[strings.Index(sel, ".")+1:]
			if _, ok := skips[sel]; ok {
				continue
			}
			g.walkType(source+"."+fname, sink+"."+fname, x, field.Type(), w, skips, generating, depth)
		}
	case *types.Slice:
		kind := g.getElemType(v.Elem(), x)

		idx := "i"
		if depth > 1 {
			idx += strconv.Itoa(depth)
		}

		// sel is only used for skips
		sel := "[i]"
		sel = sel[strings.Index(sel, ".")+1:]
		if !initial {
			sel = sink + sel
		}

		var skipSlice bool
		if skips.Contains(sel) {
			skipSlice = true
		}

		fmt.Fprintf(w, `if %s != nil {
	%s = make([]%s, len(%s))
`, source, sink, kind, source)

		fmt.Fprintf(w, `copy(%s, %s)
`, sink, source)

		var b bytes.Buffer

		if !skipSlice {
			baseSel := "[" + idx + "]"
			g.walkType(source+baseSel, sink+baseSel, x, v.Elem(), &b, skips, generating, depth)
		}

		if b.Len() > 0 {
			fmt.Fprintf(w, `    for %s := range %s {
`, idx, source)

			b.WriteTo(w)

			fmt.Fprintf(w, "}\n")
		}

		fmt.Fprintf(w, "}\n")
	case *types.Pointer:
		fmt.Fprintf(w, "if %s != nil {\n", source)

		if e, ok := v.Elem().(methoder); !ok || initial || !g.reuseDeepCopy(source, sink, e, true, generating, w) {
			kind := g.getElemType(v.Elem(), x)

			fmt.Fprintf(w, `%s = new(%s)
	*%s = *%s
`, sink, kind, sink, source)

			g.walkType(source, sink, x, v.Elem(), w, skips, generating, depth)
		}

		fmt.Fprintf(w, "}\n")
	case *types.Chan:
		kind := g.getElemType(v.Elem(), x)

		fmt.Fprintf(w, `if %s != nil {
	%s = make(chan %s, cap(%s))
}
`, source, sink, kind, source)
	case *types.Map:
		kkind := g.getElemType(v.Key(), x)
		vkind := g.getElemType(v.Elem(), x)

		key, val := "k", "v"

		if depth > 1 {
			key += strconv.Itoa(depth)
			val += strconv.Itoa(depth)
		}

		// Sel is only used for skips
		sel := "[k]"
		if !initial {
			sel = sink + sel
		}
		sel = sel[strings.Index(sel, ".")+1:]

		var skipKey, skipValue bool
		if skips.Contains(sel) {
			skipKey, skipValue = true, true
		}

		fmt.Fprintf(w, `if %s != nil {
	%s = make(map[%s]%s, len(%s))
	for %s, %s := range %s {
`, source, sink, kkind, vkind, source, key, val, source)

		ksink, vsink := key, val

		var b bytes.Buffer

		if !skipKey {
			copyKSink := selToIdent(sink) + "_" + key
			g.walkType(key, copyKSink, x, v.Key(), &b, skips, generating, depth)

			if b.Len() > 0 {
				ksink = copyKSink
				fmt.Fprintf(w, "var %s %s\n", ksink, kkind)
				b.WriteTo(w)
			}
		}

		b.Reset()

		if !skipValue {
			copyVSink := selToIdent(sink) + "_" + val
			g.walkType(val, copyVSink, x, v.Elem(), &b, skips, generating, depth)

			if b.Len() > 0 {
				vsink = copyVSink
				fmt.Fprintf(w, "var %s %s\n", vsink, vkind)
				b.WriteTo(w)
			}
		}

		fmt.Fprintf(w, "%s[%s] = %s", sink, ksink, vsink)

		fmt.Fprintf(w, "}\n}\n")
	}
}

func (g Generator) hasDeepCopy(v methoder, generating []object) (hasMethod, isPointer bool) {
	for _, t := range generating {
		if types.Identical(v, t) {
			return true, g.isPtrRecv
		}
	}

	for i := 0; i < v.NumMethods(); i++ {
		m := v.Method(i)
		if m.Name() != g.methodName {
			continue
		}

		sig, ok := m.Type().(*types.Signature)
		if !ok {
			continue
		}

		if sig.Params().Len() != 0 || sig.Results().Len() != 1 {
			continue
		}

		ret := sig.Results().At(0)
		retType, retPointer := reducePointer(ret.Type())
		sigType, _ := reducePointer(sig.Recv().Type())

		if !types.Identical(retType, sigType) {
			return false, false
		}

		return true, retPointer
	}

	return false, false
}

func (g Generator) reuseDeepCopy(source, sink string, v methoder, pointer bool, generating []object, w io.Writer) bool {
	hasMethod, isPointer := g.hasDeepCopy(v, generating)

	if hasMethod {
		if pointer == isPointer {
			fmt.Fprintf(w, "%s = %s.%s()\n", sink, source, g.methodName)
		} else if pointer {
			fmt.Fprintf(w, `retV := %s.%s()
	%s = &retV
`, source, g.methodName, sink)
		} else {
			fmt.Fprintf(w, `{
	retV := %s.%s()
	%s = *retV
}
`, source, g.methodName, sink)
		}
	}

	return hasMethod
}

func locateType(kind string, p *packages.Package) (object, error) {
	for _, t := range p.TypesInfo.Defs {
		if t == nil {
			continue
		}
		m := exprFilter(t.Type(), kind, p.Name)
		if m == nil {
			continue
		}

		return m, nil
	}

	return nil, errors.New("type not found")
}

func reducePointer(typ types.Type) (types.Type, bool) {
	if pointer, ok := typ.(pointer); ok {
		return pointer.Elem(), true
	}
	return typ, false
}

func objFromType(typ types.Type) object {
	typ, _ = reducePointer(typ)

	m, ok := typ.(object)
	if !ok {
		return nil
	}

	return m
}

func exprFilter(t types.Type, sel string, x string) object {
	m := objFromType(t)
	if m == nil {
		return nil
	}

	obj := m.Obj()
	if obj.Pkg() == nil || x != obj.Pkg().Name() || sel != obj.Name() {
		return nil
	}

	return m
}

var importSanitizerRE = regexp.MustCompile(`\W`)

func (g Generator) getElemType(t types.Type, x string) string {
	kind := types.TypeString(t, func(p *types.Package) string {
		name := p.Name()
		if name != x {
			if path, ok := g.imports[name]; ok && path != p.Path() {
				name = importSanitizerRE.ReplaceAllString(p.Path(), "_")
			}

			g.imports[name] = p.Path()
			return name
		}
		return ""
	})

	return kind
}

func selToIdent(sel string) string {
	sel = strings.ReplaceAll(sel, "]", "")

	return strings.Map(func(r rune) rune {
		switch r {
		case '[', '.':
			return '_'
		default:
			return r
		}
	}, sel)
}
