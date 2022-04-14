package compare

import (
	"cmd/compile/internal/base"
	"cmd/compile/internal/ir"
	"cmd/compile/internal/typecheck"
	"cmd/compile/internal/types"
	"fmt"
	"math/bits"
	"sort"
)

// IsRegularMemory reports whether t can be compared/hashed as regular memory.
func IsRegularMemory(t *types.Type) bool {
	a, _ := types.AlgType(t)
	return a == types.AMEM
}

// Memrun finds runs of struct fields for which memory-only algs are appropriate.
// t is the parent struct type, and start is the field index at which to start the run.
// size is the length in bytes of the memory included in the run.
// next is the index just after the end of the memory run.
func Memrun(t *types.Type, start int) (size int64, next int) {
	next = start
	for {
		next++
		if next == t.NumFields() {
			break
		}
		// Stop run after a padded field.
		if types.IsPaddedField(t, next-1) {
			break
		}
		// Also, stop before a blank or non-memory field.
		if f := t.Field(next); f.Sym.IsBlank() || !IsRegularMemory(f.Type) {
			break
		}
		// For issue 46283, don't combine fields if the resulting load would
		// require a larger alignment than the component fields.
		if base.Ctxt.Arch.Alignment > 1 {
			align := t.Alignment()
			if off := t.Field(start).Offset; off&(align-1) != 0 {
				// Offset is less aligned than the containing type.
				// Use offset to determine alignment.
				align = 1 << uint(bits.TrailingZeros64(uint64(off)))
			}
			size := t.Field(next).End() - t.Field(start).Offset
			if size > align {
				break
			}
		}
	}
	return t.Field(next-1).End() - t.Field(start).Offset, next
}

// EqCanPanic reports whether == on type t could panic (has an interface somewhere).
// t must be comparable.
func EqCanPanic(t *types.Type) bool {
	switch t.Kind() {
	default:
		return false
	case types.TINTER:
		return true
	case types.TARRAY:
		return EqCanPanic(t.Elem())
	case types.TSTRUCT:
		for _, f := range t.FieldSlice() {
			if !f.Sym.IsBlank() && EqCanPanic(f.Type) {
				return true
			}
		}
		return false
	}
}

// Struct compares two structs np and nq using the provided op.
func Struct(t *types.Type, op ir.Op, np, nq ir.Node) []ir.Node {
	// Build a list of conditions to satisfy.
	// The conditions are a list-of-lists. Conditions are reorderable
	// within each inner list. The outer lists must be evaluated in order.
	var conds [][]ir.Node
	conds = append(conds, []ir.Node{})
	and := func(n ir.Node) {
		i := len(conds) - 1
		conds[i] = append(conds[i], n)
	}

	// Walk the struct using memequal for runs of AMEM
	// and calling specific equality tests for the others.
	for i, fields := 0, t.FieldSlice(); i < len(fields); {
		f := fields[i]

		// Skip blank-named fields.
		if f.Sym.IsBlank() {
			i++
			continue
		}

		// Compare non-memory fields with field equality.
		if !IsRegularMemory(f.Type) {
			if EqCanPanic(f.Type) {
				// Enforce ordering by starting a new set of reorderable conditions.
				conds = append(conds, []ir.Node{})
			}
			p := ir.NewSelectorExpr(base.Pos, ir.OXDOT, np, f.Sym)
			q := ir.NewSelectorExpr(base.Pos, ir.OXDOT, nq, f.Sym)
			switch {
			case f.Type.IsString():
				eqlen, eqmem := EqString(p, q)
				var callmem ir.Node = eqmem
				if op != ir.OEQ {
					eqlen.SetOp(op)
					callmem = ir.NewUnaryExpr(base.Pos, ir.ONOT, eqmem)
				}
				and(eqlen)
				and(callmem)
			default:
				and(ir.NewBinaryExpr(base.Pos, op, p, q))
			}
			if EqCanPanic(f.Type) {
				// Also enforce ordering after something that can panic.
				conds = append(conds, []ir.Node{})
			}
			i++
			continue
		}

		// Find maximal length run of memory-only fields.
		size, next := Memrun(t, i)

		// TODO(rsc): All the calls to newname are wrong for
		// cross-package unexported fields.
		if s := fields[i:next]; len(s) <= 2 {
			// Two or fewer fields: use plain field equality.
			for _, f := range s {
				and(eqfield(np, nq, op, f.Sym))
			}
		} else {
			// More than two fields: use memequal.
			cc := eqmem(np, nq, f.Sym, size)
			if op != ir.OEQ {
				cc = ir.NewUnaryExpr(base.Pos, ir.ONOT, cc)
			}
			and(cc)
		}
		i = next
	}

	// Sort conditions to put runtime calls last.
	// Preserve the rest of the ordering.
	var flatConds []ir.Node
	for _, c := range conds {
		isCall := func(n ir.Node) bool {
			return n.Op() == ir.OCALL || n.Op() == ir.OCALLFUNC
		}
		sort.SliceStable(c, func(i, j int) bool {
			return !isCall(c[i]) && isCall(c[j])
		})
		flatConds = append(flatConds, c...)
	}
	return flatConds
}

// EqString returns the nodes
//
//	len(s) == len(t)
//
// and
//
//	memequal(s.ptr, t.ptr, len(s))
//
// which can be used to construct string equality comparison.
// eqlen must be evaluated before eqmem, and shortcircuiting is required.
func EqString(s, t ir.Node) (eqlen *ir.BinaryExpr, eqmem *ir.CallExpr) {
	s = typecheck.Conv(s, types.Types[types.TSTRING])
	t = typecheck.Conv(t, types.Types[types.TSTRING])
	sptr := ir.NewUnaryExpr(base.Pos, ir.OSPTR, s)
	tptr := ir.NewUnaryExpr(base.Pos, ir.OSPTR, t)
	slen := typecheck.Conv(ir.NewUnaryExpr(base.Pos, ir.OLEN, s), types.Types[types.TUINTPTR])
	tlen := typecheck.Conv(ir.NewUnaryExpr(base.Pos, ir.OLEN, t), types.Types[types.TUINTPTR])

	fn := typecheck.LookupRuntime("memequal")
	fn = typecheck.SubstArgTypes(fn, types.Types[types.TUINT8], types.Types[types.TUINT8])
	call := typecheck.Call(base.Pos, fn, []ir.Node{sptr, tptr, ir.Copy(slen)}, false).(*ir.CallExpr)

	cmp := ir.NewBinaryExpr(base.Pos, ir.OEQ, slen, tlen)
	cmp = typecheck.Expr(cmp).(*ir.BinaryExpr)
	cmp.SetType(types.Types[types.TBOOL])
	return cmp, call
}

// EqInterface returns the nodes
//
//	s.tab == t.tab (or s.typ == t.typ, as appropriate)
//
// and
//
//	ifaceeq(s.tab, s.data, t.data) (or efaceeq(s.typ, s.data, t.data), as appropriate)
//
// which can be used to construct interface equality comparison.
// eqtab must be evaluated before eqdata, and shortcircuiting is required.
func EqInterface(s, t ir.Node) (eqtab *ir.BinaryExpr, eqdata *ir.CallExpr) {
	if !types.Identical(s.Type(), t.Type()) {
		base.Fatalf("EqInterface %v %v", s.Type(), t.Type())
	}
	// func ifaceeq(tab *uintptr, x, y unsafe.Pointer) (ret bool)
	// func efaceeq(typ *uintptr, x, y unsafe.Pointer) (ret bool)
	var fn ir.Node
	if s.Type().IsEmptyInterface() {
		fn = typecheck.LookupRuntime("efaceeq")
	} else {
		fn = typecheck.LookupRuntime("ifaceeq")
	}

	stab := ir.NewUnaryExpr(base.Pos, ir.OITAB, s)
	ttab := ir.NewUnaryExpr(base.Pos, ir.OITAB, t)
	sdata := ir.NewUnaryExpr(base.Pos, ir.OIDATA, s)
	tdata := ir.NewUnaryExpr(base.Pos, ir.OIDATA, t)
	sdata.SetType(types.Types[types.TUNSAFEPTR])
	tdata.SetType(types.Types[types.TUNSAFEPTR])
	sdata.SetTypecheck(1)
	tdata.SetTypecheck(1)

	call := typecheck.Call(base.Pos, fn, []ir.Node{stab, sdata, tdata}, false).(*ir.CallExpr)

	cmp := ir.NewBinaryExpr(base.Pos, ir.OEQ, stab, ttab)
	cmp = typecheck.Expr(cmp).(*ir.BinaryExpr)
	cmp.SetType(types.Types[types.TBOOL])
	return cmp, call
}

// eqfield returns the node
//
//	p.field == q.field
func eqfield(p ir.Node, q ir.Node, op ir.Op, field *types.Sym) ir.Node {
	nx := ir.NewSelectorExpr(base.Pos, ir.OXDOT, p, field)
	ny := ir.NewSelectorExpr(base.Pos, ir.OXDOT, q, field)
	ne := ir.NewBinaryExpr(base.Pos, op, nx, ny)
	return ne
}

// eqmem returns the node
//
//	memequal(&p.field, &q.field, size])
func eqmem(p ir.Node, q ir.Node, field *types.Sym, size int64) ir.Node {
	nx := typecheck.Expr(typecheck.NodAddr(ir.NewSelectorExpr(base.Pos, ir.OXDOT, p, field)))
	ny := typecheck.Expr(typecheck.NodAddr(ir.NewSelectorExpr(base.Pos, ir.OXDOT, q, field)))

	fn, needsize := eqmemfunc(size, nx.Type().Elem())
	call := ir.NewCallExpr(base.Pos, ir.OCALL, fn, nil)
	call.Args.Append(nx)
	call.Args.Append(ny)
	if needsize {
		call.Args.Append(ir.NewInt(size))
	}

	return call
}

func eqmemfunc(size int64, t *types.Type) (fn *ir.Name, needsize bool) {
	switch size {
	default:
		fn = typecheck.LookupRuntime("memequal")
		needsize = true
	case 1, 2, 4, 8, 16:
		buf := fmt.Sprintf("memequal%d", int(size)*8)
		fn = typecheck.LookupRuntime(buf)
	}

	fn = typecheck.SubstArgTypes(fn, t, t)
	return fn, needsize
}
