package pythonutil

import "math/big"

// Folds the negative and complex number literals of match patterns into constants, as the compiler does before it checks the patterns
func foldMatchConstants(tree *Node) {
	for n := range Walk(tree) {
		switch n.Type {
		case "MatchValue":
			foldMatchConst(n.Child("value"))
		case "MatchMapping":
			for _, k := range n.Children("keys") {
				foldMatchConst(k)
			}
		}
	}
}

// Turns a negated number or a real plus or minus imaginary number into the Constant it evaluates to
func foldMatchConst(n *Node) {
	switch n.Type {
	case "UnaryOp":
		if operand := n.Child("operand"); n.Child("op").Is("USub") && operand.Is("Constant") {
			switch v := operand.Get("value").(type) {
			case int64, *big.Int, float64, complex128:
				n.becomeConstant(negateNumber(v))
			}
		}
	case "BinOp":
		op := n.Child("op")
		if right := n.Child("right"); op.Is("Add", "Sub") && right.Is("Constant") {
			foldMatchConst(n.Child("left"))
			if left := n.Child("left"); left.Is("Constant") {
				if v, ok := addNumbers(left.Get("value"), right.Get("value"), op.Is("Sub")); ok {
					n.becomeConstant(v)
				}
			}
		}
	}
}

// Turns the node into a Constant holding v, keeping its position
func (n *Node) becomeConstant(v any) {
	n.Type, n.spec, n.vals = "Constant", specs()["Constant"], []any{v, nil}
}

// Sum or difference of two numbers with Python's mixed real and complex rules, false where Python fails
func addNumbers(l, r any, sub bool) (any, bool) {
	lc, lIsC := l.(complex128)
	rc, rIsC := r.(complex128)
	if sub && rIsC {
		rc = -rc
	}
	switch {
	case lIsC && rIsC:
		return lc + rc, true
	case rIsC:
		x, ok := realFloat(l)
		if !ok {
			return nil, false
		}
		return complex(x+real(rc), imag(rc)), true
	case lIsC:
		y, ok := realFloat(r)
		if !ok {
			return nil, false
		}
		if sub {
			y = -y
		}
		return complex(real(lc)+y, imag(lc)), true
	}
	if li, ok := bigInt(l); ok {
		if ri, ok := bigInt(r); ok {
			out := new(big.Int)
			if sub {
				out.Sub(li, ri)
			} else {
				out.Add(li, ri)
			}
			if out.IsInt64() {
				return out.Int64(), true
			}
			return out, true
		}
	}
	x, okx := realFloat(l)
	y, oky := realFloat(r)
	if !okx || !oky {
		return nil, false
	}
	if sub {
		return x - y, true
	}
	return x + y, true
}

func bigInt(v any) (*big.Int, bool) {
	switch x := v.(type) {
	case int64:
		return big.NewInt(x), true
	case *big.Int:
		return x, true
	}
	return nil, false
}
