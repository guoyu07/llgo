/*
Copyright (c) 2011, 2012 Andrew Wilkins <axwalk@gmail.com>

Permission is hereby granted, free of charge, to any person obtaining a copy of
this software and associated documentation files (the "Software"), to deal in
the Software without restriction, including without limitation the rights to
use, copy, modify, merge, publish, distribute, sublicense, and/or sell copies
of the Software, and to permit persons to whom the Software is furnished to do
so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package llgo

import (
	"fmt"
	"github.com/axw/gollvm/llvm"
	"github.com/axw/llgo/types"
	"go/token"
	"math"
	"math/big"
)

var (
	maxBigInt32 = big.NewInt(math.MaxInt32)
	minBigInt32 = big.NewInt(math.MinInt32)
)

// Value is an interface for representing values returned by Go expressions.
type Value interface {
	// BinaryOp applies the specified binary operator to this value and the
	// specified right-hand operand, and returns a new Value.
	BinaryOp(op token.Token, rhs Value) Value

	// UnaryOp applies the specified unary operator and returns a new Value.
	UnaryOp(op token.Token) Value

	// Convert returns a new Value which has been converted to the specified
	// type.
	Convert(typ types.Type) Value

	// LLVMValue returns an llvm.Value for this value.
	LLVMValue() llvm.Value

	// Type returns the Type of the value.
	Type() types.Type
}

type LLVMValue struct {
	compiler *compiler
	value    llvm.Value
	typ      types.Type
	indirect bool
	address  *LLVMValue // Value that dereferenced to this value.
	receiver *LLVMValue
}

type ConstValue struct {
	types.Const
	compiler *compiler
	typ      *types.Basic
}

// Create a new dynamic value from a (LLVM Builder, LLVM Value, Type) triplet.
func (c *compiler) NewLLVMValue(v llvm.Value, t types.Type) *LLVMValue {
	return &LLVMValue{c, v, t, false, nil, nil}
}

// Create a new constant value from a literal with accompanying type, as
// provided by ast.BasicLit.
func (c *compiler) NewConstValue(tok token.Token, lit string) ConstValue {
	var typ *types.Basic
	switch tok {
	case token.INT:
		typ = &types.Basic{Kind: types.UntypedIntKind}
	case token.FLOAT:
		typ = &types.Basic{Kind: types.UntypedFloatKind}
	case token.IMAG:
		typ = &types.Basic{Kind: types.UntypedComplexKind}
	case token.CHAR:
		typ = types.Rune.Underlying.(*types.Basic)
	case token.STRING:
		typ = types.String.Underlying.(*types.Basic)
	}
	return ConstValue{*types.MakeConst(tok, lit), c, typ}
}

///////////////////////////////////////////////////////////////////////////////
// LLVMValue methods

func (lhs *LLVMValue) BinaryOp(op token.Token, rhs_ Value) Value {
	// Deref lhs, if it's indirect.
	if lhs.indirect {
		lhs = lhs.Deref()
	}

	var result llvm.Value
	c := lhs.compiler
	b := lhs.compiler.builder

	switch rhs := rhs_.(type) {
	case *LLVMValue:
		// Deref rhs, if it's indirect.
		if rhs.indirect {
			rhs = rhs.Deref()
		}

		// Special case for structs.
		// TODO handle strings as an even more special case.
		if lhs.value.Type().TypeKind() == llvm.StructTypeKind {
			// TODO check types are the same.
			struct_type := lhs.Type()
			if name, ok := struct_type.(*types.Name); ok {
				struct_type = name.Underlying
			}

			element_types_count := lhs.value.Type().StructElementTypesCount()
			var t types.Type = &types.Bad{}
			var struct_fields types.ObjList
			if s, ok := struct_type.(*types.Struct); ok {
				struct_fields = s.Fields
			}

			if element_types_count > 0 {
				if struct_fields != nil {
					t = c.ObjGetType(struct_fields[0])
				}
				first_lhs := c.NewLLVMValue(b.CreateExtractValue(lhs.value, 0, ""), t)
				first_rhs := c.NewLLVMValue(b.CreateExtractValue(rhs.value, 0, ""), t)
				first := first_lhs.BinaryOp(op, first_rhs)
				result := first

				var logical_op token.Token
				switch op {
				case token.EQL:
					logical_op = token.LAND
				case token.NEQ:
					logical_op = token.LOR
				default:
					panic("Unexpected operator")
				}

				for i := 1; i < element_types_count; i++ {
					if struct_fields != nil {
						t = c.ObjGetType(struct_fields[1])
					}
					next_lhs := c.NewLLVMValue(b.CreateExtractValue(lhs.value, i, ""), t)
					next_rhs := c.NewLLVMValue(b.CreateExtractValue(rhs.value, i, ""), t)
					next := next_lhs.BinaryOp(op, next_rhs)
					result = result.BinaryOp(logical_op, next)
				}
				return result
			}
		}

		switch op {
		case token.MUL:
			result = b.CreateMul(lhs.value, rhs.value, "")
		case token.QUO:
			result = b.CreateUDiv(lhs.value, rhs.value, "")
		case token.ADD:
			result = b.CreateAdd(lhs.value, rhs.value, "")
		case token.SUB:
			result = b.CreateSub(lhs.value, rhs.value, "")
		case token.NEQ:
			result = b.CreateICmp(llvm.IntNE, lhs.value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.EQL:
			result = b.CreateICmp(llvm.IntEQ, lhs.value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LSS:
			result = b.CreateICmp(llvm.IntULT, lhs.value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LEQ: // TODO signed/unsigned
			result = b.CreateICmp(llvm.IntULE, lhs.value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LAND:
			result = b.CreateAnd(lhs.value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LOR:
			result = b.CreateOr(lhs.value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		default:
			panic(fmt.Sprint("Unimplemented operator: ", op))
		}
		return lhs.compiler.NewLLVMValue(result, lhs.typ)
	case ConstValue:
		// Cast untyped rhs to lhs type.
		switch rhs.typ.Kind {
		case types.UntypedIntKind:
			fallthrough
		case types.UntypedFloatKind:
			fallthrough
		case types.UntypedComplexKind:
			rhs = rhs.Convert(lhs.Type()).(ConstValue)
		case types.NilKind:
			// The conversion will result in an *LLVMValue.
			// XXX Perhaps this is too lazy. We could optimise some
			// comparisons, e.g. interface == nil could be optimised
			// to only compare the type field.
			rhs_llvm := rhs.Convert(lhs.Type()).(*LLVMValue)
			return lhs.BinaryOp(op, rhs_llvm)
		}
		rhs_value := rhs.LLVMValue()

		switch op {
		case token.MUL:
			result = b.CreateMul(lhs.value, rhs_value, "")
		case token.QUO:
			result = b.CreateUDiv(lhs.value, rhs_value, "")
		case token.ADD:
			result = b.CreateAdd(lhs.value, rhs_value, "")
		case token.SUB:
			result = b.CreateSub(lhs.value, rhs_value, "")
		case token.NEQ:
			result = b.CreateICmp(llvm.IntNE, lhs.value, rhs_value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.EQL:
			result = b.CreateICmp(llvm.IntEQ, lhs.value, rhs_value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LSS:
			result = b.CreateICmp(llvm.IntULT, lhs.value, rhs_value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LEQ: // TODO signed/unsigned
			result = b.CreateICmp(llvm.IntULE, lhs.value, rhs_value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LAND:
			result = b.CreateAnd(lhs.value, rhs_value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LOR:
			result = b.CreateOr(lhs.value, rhs_value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		default:
			panic(fmt.Sprint("Unimplemented operator: ", op))
		}
		return lhs.compiler.NewLLVMValue(result, lhs.typ)
	}
	panic("unreachable")
}

func (v *LLVMValue) UnaryOp(op token.Token) Value {
	b := v.compiler.builder
	switch op {
	case token.SUB:
		if v.indirect {
			v2 := v.Deref()
			return v.compiler.NewLLVMValue(b.CreateNeg(v2.value, ""), v2.typ)
		}
		return v.compiler.NewLLVMValue(b.CreateNeg(v.value, ""), v.typ)
	case token.ADD:
		return v // No-op
	case token.AND:
		if v.indirect {
			return v.compiler.NewLLVMValue(v.value, v.typ)
		}
		return v.compiler.NewLLVMValue(v.address.LLVMValue(),
			&types.Pointer{Base: v.typ})
	default:
		panic("Unhandled operator: ") // + expr.Op)
	}
	panic("unreachable")
}

func (v *LLVMValue) Convert(dst_typ types.Type) Value {
	// If it's a stack allocated value, we'll want to compare the
	// value type, not the pointer type.
	src_typ := v.typ
	if v.indirect {
		src_typ = types.Deref(src_typ)
	}

	// Get the underlying type, if any.
	orig_dst_typ := dst_typ
	if name, isname := dst_typ.(*types.Name); isname {
		dst_typ = types.Underlying(name)
	}

	// Get the underlying type, if any.
	if name, isname := src_typ.(*types.Name); isname {
		src_typ = types.Underlying(name)
	}

	// Identical (underlying) types? Just swap in the destination type.
	if types.Identical(src_typ, dst_typ) {
		dst_typ = orig_dst_typ
		if v.indirect {
			dst_typ = &types.Pointer{Base: dst_typ}
		}
		// XXX do we need to copy address/receiver here?
		newv := v.compiler.NewLLVMValue(v.value, dst_typ)
		newv.indirect = true
		return newv
	}

	// Convert from an interface type.
	if _, isinterface := src_typ.(*types.Interface); isinterface {
		if interface_, isinterface := dst_typ.(*types.Interface); isinterface {
			return v.convertI2I(interface_)
		}
		// TODO I2V
	}

	// Converting to an interface type.
	if interface_, isinterface := dst_typ.(*types.Interface); isinterface {
		return v.convertV2I(interface_)
	}

	/*
	   value_type := value.Type()
	   switch value_type.TypeKind() {
	   case llvm.IntegerTypeKind:
	       switch totype.TypeKind() {
	       case llvm.IntegerTypeKind:
	           //delta := value_type.IntTypeWidth() - totype.IntTypeWidth()
	           //var 
	           switch {
	           case delta == 0: return value
	           // TODO handle signed/unsigned (SExt/ZExt)
	           case delta < 0: return c.compiler.builder.CreateZExt(value, totype, "")
	           case delta > 0: return c.compiler.builder.CreateTrunc(value, totype, "")
	           }
	           return LLVMValue{lhs.compiler.builder, value}
	       }
	   }
	*/
	panic(fmt.Sprint("unimplemented conversion: ", v.typ, " -> ", orig_dst_typ))
}

func (v *LLVMValue) LLVMValue() llvm.Value {
	return v.value
}

func (v *LLVMValue) Type() types.Type {
	return v.typ
}

// Dereference an LLVMValue, producing a new LLVMValue.
func (v *LLVMValue) Deref() *LLVMValue {
	llvm_value := v.compiler.builder.CreateLoad(v.value, "")
	value := v.compiler.NewLLVMValue(llvm_value, types.Deref(v.typ))
	value.address = v
	return value
}

///////////////////////////////////////////////////////////////////////////////
// ConstValue methods.

func (lhs ConstValue) BinaryOp(op token.Token, rhs_ Value) Value {
	switch rhs := rhs_.(type) {
	case *LLVMValue:
		// Deref rhs, if it's indirect.
		if rhs.indirect {
			rhs = rhs.Deref()
		}

		// Cast untyped lhs to rhs type.
		switch lhs.typ.Kind {
		case types.UntypedIntKind:
			fallthrough
		case types.UntypedFloatKind:
			fallthrough
		case types.UntypedComplexKind:
			lhs = lhs.Convert(rhs.Type()).(ConstValue)
		}
		lhs_value := lhs.LLVMValue()

		b := rhs.compiler.builder
		var result llvm.Value
		switch op {
		case token.MUL:
			result = b.CreateMul(lhs_value, rhs.value, "")
		case token.QUO:
			result = b.CreateUDiv(lhs_value, rhs.value, "")
		case token.ADD:
			result = b.CreateAdd(lhs_value, rhs.value, "")
		case token.SUB:
			result = b.CreateSub(lhs_value, rhs.value, "")
		case token.NEQ:
			result = b.CreateICmp(llvm.IntNE, lhs_value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.EQL:
			result = b.CreateICmp(llvm.IntEQ, lhs_value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LSS:
			result = b.CreateICmp(llvm.IntULT, lhs_value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LAND:
			result = b.CreateAnd(lhs_value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		case token.LOR:
			result = b.CreateOr(lhs_value, rhs.value, "")
			return lhs.compiler.NewLLVMValue(result, types.Bool)
		default:
			panic(fmt.Sprint("Unimplemented operator: ", op))
		}
		return rhs.compiler.NewLLVMValue(result, lhs.typ)
	case ConstValue:
		// TODO Check if either one is untyped, and convert to the other's
		// type.
		c := lhs.compiler
		typ := lhs.typ
		return ConstValue{*lhs.Const.BinaryOp(op, &rhs.Const), c, typ}
	}
	panic("unimplemented")
}

func (v ConstValue) UnaryOp(op token.Token) Value {
	return ConstValue{*v.Const.UnaryOp(op), v.compiler, v.typ}
}

func (v ConstValue) Convert(dst_typ types.Type) Value {
	// Get the underlying type, if any.
	if name, isname := dst_typ.(*types.Name); isname {
		dst_typ = types.Underlying(name)
	}

	if !types.Identical(v.typ, dst_typ) {
		// Get the Basic type.
		if name, isname := dst_typ.(*types.Name); isname {
			dst_typ = name.Underlying
		}

		compiler := v.compiler
		if basic, ok := dst_typ.(*types.Basic); ok {
			return ConstValue{*v.Const.Convert(&dst_typ), compiler, basic}
		} else {
			// Special case for 'nil'
			if v.typ.Kind == types.NilKind {
				zero := llvm.ConstNull(compiler.types.ToLLVM(dst_typ))
				return compiler.NewLLVMValue(zero, dst_typ)
			}
			panic("unhandled conversion")
		}
	} else {
		// TODO convert to dst type. ConstValue may need to change to allow
		// storage of types other than Basic.
	}
	return v
}

func (v ConstValue) LLVMValue() llvm.Value {
	// From the language spec:
	//   If the type is absent and the corresponding expression evaluates to
	//   an untyped constant, the type of the declared variable is bool, int,
	//   float64, or string respectively, depending on whether the value is
	//   a boolean, integer, floating-point, or string constant.

	switch v.typ.Kind {
	case types.UntypedIntKind:
		// TODO 32/64bit
		int_val := v.Val.(*big.Int)
		if int_val.Cmp(maxBigInt32) > 0 || int_val.Cmp(minBigInt32) < 0 {
			panic(fmt.Sprint("const ", int_val, " overflows int"))
		}
		return llvm.ConstInt(llvm.Int32Type(), uint64(v.Int64()), false)
	case types.UntypedFloatKind:
		fallthrough
	case types.UntypedComplexKind:
		panic("Attempting to take LLVM value of untyped constant")
	case types.Int32Kind, types.Uint32Kind:
		// XXX rune
		return llvm.ConstInt(llvm.Int32Type(), uint64(v.Int64()), false)
	case types.Int16Kind, types.Uint16Kind:
		return llvm.ConstInt(llvm.Int16Type(), uint64(v.Int64()), false)
	case types.StringKind:
		strval := (v.Val).(string)
		ptr := v.compiler.builder.CreateGlobalStringPtr(strval, "")
		len_ := llvm.ConstInt(llvm.Int32Type(), uint64(len(strval)), false)
		return llvm.ConstStruct([]llvm.Value{ptr, len_}, false)
	case types.BoolKind:
		if v := v.Val.(bool); v {
			return llvm.ConstAllOnes(llvm.Int1Type())
		}
		return llvm.ConstNull(llvm.Int1Type())
	}
	panic("Unhandled type")
}

func (v ConstValue) Type() types.Type {
	// TODO convert untyped to typed?
	switch v.typ.Kind {
	case types.UntypedIntKind:
		return types.Int
	}
	return v.typ
}

func (v ConstValue) Int64() int64 {
	int_val := v.Val.(*big.Int)
	return int_val.Int64()
}

// vim: set ft=go :