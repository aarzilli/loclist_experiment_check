package main

import (
	"debug/elf"
	"debug/dwarf"
	"strings"
	"fmt"
	"io"
	"sort"
	"os"
	
	"github.com/go-delve/delve/pkg/dwarf/loclist"
	"github.com/go-delve/delve/pkg/dwarf/godwarf"
)

func must(err error) {
	if err != nil {
		panic(err)
	}
}

const trace = false

func main() {
	efh, err := elf.Open("compile")
	must(err)
	defer efh.Close()
	dw, err := efh.DWARF()
	must(err)
	
	debugLocBytes, err := godwarf.GetDebugSectionElf(efh, "loc")
	must(err)
	loclistReader := loclist.New(debugLocBytes, 8)
	
	rdr := dw.Reader()
	var cu *dwarf.Entry
	var lrdr *dwarf.LineReader
	var cubase uint64
	entryLoop:
	for {
		e, err := rdr.Next()
		must(err)
		if e == nil {
			break
		}
		
		switch e.Tag {
		case dwarf.TagCompileUnit:
			cu = e
			prod, ok := e.Val(dwarf.AttrProducer).(string)
			if !ok {
				rdr.SkipChildren()
				continue entryLoop
			}
			if !strings.Contains(prod, "Go") {
				rdr.SkipChildren()
				continue entryLoop
			}
			if !strings.Contains(prod, "-N -l") {
				rdr.SkipChildren()
				continue entryLoop
			}
			lrdr, err = dw.LineReader(cu)
			must(err)
			
			rngs, err := dw.Ranges(cu)
			must(err)
			if len(rngs) == 0 {
				rdr.SkipChildren()
				continue entryLoop
			}
			cubase = rngs[0][0]
			
		case dwarf.TagSubprogram:
			dofunc(dw, rdr, e, lrdr, loclistReader, cubase)
		
		default:
			rdr.SkipChildren()
		}
	}
}

func dofunc(dw *dwarf.Data, rdr *dwarf.Reader, e *dwarf.Entry, lrdr *dwarf.LineReader, loclist *loclist.Reader, cubase uint64) {
	name, _ := e.Val(dwarf.AttrName).(string)
	
	lowpc, ok1 := e.Val(dwarf.AttrLowpc).(uint64)
	highpc, ok2 := e.Val(dwarf.AttrHighpc).(uint64)
	if !ok1 || !ok2 {
		rdr.SkipChildren()
		return
	}
	
	stmts := []dwarf.LineEntry{}
	
	var lne dwarf.LineEntry
	
	//lrdr.Reset()
	err := lrdr.SeekPC(lowpc, &lne)
	if err != dwarf.ErrUnknownPC {
		lrdr.Reset()
		must(lrdr.SeekPC(lowpc, &lne))
	}
	
	if lne.File.Name == "<autogenerated>" {
		rdr.SkipChildren()
		return
	}
	
	for lne.Address < highpc {
		if lne.IsStmt {
			stmts = append(stmts, lne)
		}
		err := lrdr.Next(&lne)
		if err != nil {
			if err == io.EOF {
				break
			}
			must(err)
		}
	}
	
	if trace {
		fmt.Printf("dofunc %s\n", name)
	}
	
	lexicalBlockStack := []LexicalBlock{}
	
	funcLoop:
	for {
		e, err := rdr.Next()
		must(err)
		if e == nil {
			panic("unexpected end")
		}
		
		switch e.Tag {
		case 0:
			if len(lexicalBlockStack) == 0 {
				break funcLoop
			}
			lexicalBlockStack = lexicalBlockStack[:len(lexicalBlockStack)-1]
		case dwarf.TagLexDwarfBlock:
			lexicalBlockStack = append(lexicalBlockStack, makeLexicalBlock(dw, e, stmts))
		case dwarf.TagVariable, dwarf.TagFormalParameter:
			lns := stmts
			if len(lexicalBlockStack) > 0 {
				lns = lexicalBlockStack[len(lexicalBlockStack)-1].stmts
			}
			dovar(name, lns, e, loclist, cubase)
		default:
			rdr.SkipChildren()
		}
	}		
}

type LexicalBlock struct {
	rngs [][2]uint64
	stmts []dwarf.LineEntry
}

func makeLexicalBlock(dw *dwarf.Data, e *dwarf.Entry, stmts []dwarf.LineEntry) LexicalBlock {
	var r LexicalBlock
	var err error
	r.rngs, err = dw.Ranges(e)
	must(err)
	r.stmts = filterInsideRanges(stmts, r.rngs)
	return r
}

func filterInsideRanges(stmts []dwarf.LineEntry, rngs [][2]uint64) []dwarf.LineEntry {
	r := make([]dwarf.LineEntry, 0, len(stmts))
	for i := range stmts {
		if rangesContains(rngs, stmts[i].Address) {
			r = append(r, stmts[i])
		}
	}
	return r
}

func rangesContains(rngs [][2]uint64, addr uint64) bool {
	for _, rng := range rngs {
		if addr >= rng[0] && addr < rng[1] {
			return true
		}
	}
	return false
}

func dovar(fnname string, stmts []dwarf.LineEntry, e *dwarf.Entry, loclist *loclist.Reader, cubase uint64) {
	varname, _ := e.Val(dwarf.AttrName).(string)
	loc := e.Val(dwarf.AttrLocation)
	if _, isblock := loc.([]byte); isblock {
		return
	}
	
	loclistOff, ok := loc.(int64)
	if !ok {
		panic("unknown loclist type")
	}
	
	declLine, ok := e.Val(dwarf.AttrDeclLine).(int64)
	if !ok {
		return
	}
	
	if trace {
		fmt.Printf("\tvariable %s\n", varname)
	}
	
	// stmtsFromLexicalBlock is the statements where the variable is readable
	// as deduced from the declaration line and the DW_AT_ranges attribute of
	// the variable's lexical block. The strategy used here is the same as
	// delve, with the exception that the declaration line is excluded (because
	// DW_AT_location will give a different result for that line).
	stmtsFromLexicalBlock := filterAfterLine(stmts, declLine)
	
	llrngs	:= loclistRangesAtOffset(cubase, loclist, loclistOff)
	
	// stmtsFromLoclist is the statements where the variable is readable as
	// deduced from its loclist entry.
	stmtsFromLoclist := filterAfterLine(filterInsideRanges(stmts, llrngs), declLine)
	
	sort.Sort(stmtsByAddress(stmtsFromLexicalBlock))
	sort.Sort(stmtsByAddress(stmtsFromLoclist))
	
	missing := []dwarf.LineEntry{}
	excess := []dwarf.LineEntry{}
	
	
	i, j := 0, 0
	
	for {
		if i >= len(stmtsFromLexicalBlock) {
			break
		}
		if j >= len(stmtsFromLoclist) {
			break
		}
		switch {
		case stmtsFromLexicalBlock[i].Address == stmtsFromLoclist[j].Address:
			i++
			j++
		case stmtsFromLexicalBlock[i].Address < stmtsFromLoclist[j].Address:
			missing = append(missing, stmtsFromLexicalBlock[i])
			i++
		case stmtsFromLexicalBlock[i].Address > stmtsFromLoclist[j].Address:
			excess = append(excess, stmtsFromLoclist[j])
			j++
		}
	}
	
	for i < len(stmtsFromLexicalBlock) {	
		missing = append(missing, stmtsFromLexicalBlock[i])
		i++ 
	}
	
	for j < len(stmtsFromLoclist) {
		excess = append(excess, stmtsFromLoclist[j])
		j++
	}
	
	if len(missing) > 0 {
		if !trace {
			fmt.Printf("Function %s, variable %s missing in statements:\n", fnname, varname)
		}
		for _, miss := range missing {
			fmt.Printf("\t%s:%d at %#x\n", miss.File.Name, miss.Line, miss.Address)
		}
		fmt.Printf("\n")
	}
	if len(excess) > 0 {
		fmt.Printf("\tEXCESS %d\n", len(excess))
		os.Exit(1)
	}	
	
	//TODO:check that no weird shit appears in the loclists
}

func filterAfterLine(stmts []dwarf.LineEntry, declLine int64) []dwarf.LineEntry {
	r := make([]dwarf.LineEntry, 0, len(stmts))
	for _, stmt := range stmts {
		if int64(stmt.Line) > declLine {
			r = append(r, stmt)
		}
	}
	return r
}

func loclistRangesAtOffset(cubase uint64, ll *loclist.Reader, off int64) [][2]uint64 {
	var e loclist.Entry
	r := [][2]uint64{}
	
	base := cubase
	ll.Seek(int(off))
	for ll.Next(&e) {
		if e.BaseAddressSelection() {
			base = e.HighPC
			continue
		}
		r = append(r, [2]uint64{e.LowPC + base, e.HighPC + base})
	}
	return r
}

type stmtsByAddress []dwarf.LineEntry

func (v stmtsByAddress) Len() int { return len(v) }
func (v stmtsByAddress) Swap(i, j int) { v[i], v[j] = v[j], v[i] }
func (v stmtsByAddress) Less(i, j int) bool { return v[i].Address < v[j].Address }
