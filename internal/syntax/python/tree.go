// Parses Python source into a typed syntax tree with the reference syntax errors, limits and literal evaluation
package python

// Source range with one based lines and zero based UTF-8 byte columns
type Pos struct {
	Line, Col, EndLine, EndCol int
}

// Range itself, so every located node exposes its source range through its interface
func (p Pos) Position() Pos { return p }

// Any element of the syntax tree
type Node interface{ node() }

// Statement
type Stmt interface {
	Node
	Position() Pos
	stmt()
}

// Expression
type Expr interface {
	Node
	Position() Pos
	expr()
}

// One pattern of a match case
type Pattern interface {
	Node
	Position() Pos
	pattern()
}

// Type parameter of a generic function, class or type alias
type TypeParam interface {
	Node
	Position() Pos
	typeParam()
}

// Whether a target expression is read, assigned or deleted
type Context uint8

// Expression contexts
const (
	Load Context = iota
	Store
	Del
)

// Operator joining the operands of a BoolOp
type BoolOperator uint8

// Boolean operators
const (
	And BoolOperator = iota
	Or
)

// Operator combining the operands of a BinOp or an AugAssign
type BinaryOperator uint8

// Binary operators
const (
	Add BinaryOperator = iota
	Sub
	Mult
	MatMult
	Div
	Mod
	Pow
	LShift
	RShift
	BitOr
	BitXor
	BitAnd
	FloorDiv
)

// Operator applied to the operand of a UnaryOp
type UnaryOperator uint8

// Unary operators
const (
	Invert UnaryOperator = iota
	Not
	UAdd
	USub
)

// Operator linking two operands of a Compare
type CmpOperator uint8

// Comparison operators
const (
	Eq CmpOperator = iota
	NotEq
	Lt
	LtE
	Gt
	GtE
	Is
	IsNot
	In
	NotIn
)

// Statements of a source file
type Module struct {
	Body []Stmt
}

// Def or async def statement
type FunctionDef struct {
	Pos
	Name       string
	Args       *Arguments
	Body       []Stmt
	Decorators []Expr
	Returns    Expr
	TypeParams []TypeParam
	Async      bool
}

// Class statement
type ClassDef struct {
	Pos
	Name       string
	Bases      []Expr
	Keywords   []*Keyword
	Body       []Stmt
	Decorators []Expr
	TypeParams []TypeParam
}

// return statement with an optional value
type Return struct {
	Pos
	Value Expr
}

// Del statement
type Delete struct {
	Pos
	Targets []Expr
}

// Assignment with one or more chained targets
type Assign struct {
	Pos
	Targets []Expr
	Value   Expr
}

// Type statement
type TypeAlias struct {
	Pos
	Name       *Name
	TypeParams []TypeParam
	Value      Expr
}

// Augmented assignment such as x += 1
type AugAssign struct {
	Pos
	Target Expr
	Op     BinaryOperator
	Value  Expr
}

// Annotated assignment, Simple set for a bare unparenthesized name target
type AnnAssign struct {
	Pos
	Target     Expr
	Annotation Expr
	Value      Expr
	Simple     bool
}

// for or async for loop
type For struct {
	Pos
	Target Expr
	Iter   Expr
	Body   []Stmt
	Orelse []Stmt
	Async  bool
}

// while loop
type While struct {
	Pos
	Test   Expr
	Body   []Stmt
	Orelse []Stmt
}

// if statement, an elif chain nesting one if statement in Orelse
type If struct {
	Pos
	Test   Expr
	Body   []Stmt
	Orelse []Stmt
}

// with or async with statement
type With struct {
	Pos
	Items []*WithItem
	Body  []Stmt
	Async bool
}

// match statement
type Match struct {
	Pos
	Subject Expr
	Cases   []*MatchCase
}

// raise statement with optional exception and cause
type Raise struct {
	Pos
	Exc   Expr
	Cause Expr
}

// try statement, Star set when its handlers are except* clauses
type Try struct {
	Pos
	Body      []Stmt
	Handlers  []*ExceptHandler
	Orelse    []Stmt
	Finalbody []Stmt
	Star      bool
}

// assert statement with an optional message
type Assert struct {
	Pos
	Test Expr
	Msg  Expr
}

// import statement
type Import struct {
	Pos
	Names []*Alias
}

// From import statement, Module empty for a purely relative import
type ImportFrom struct {
	Pos
	Module string
	Names  []*Alias
	Level  int
}

// global declaration
type Global struct {
	Pos
	Names []string
}

// nonlocal declaration
type Nonlocal struct {
	Pos
	Names []string
}

// Expression used as a statement
type ExprStmt struct {
	Pos
	Value Expr
}

// pass statement
type Pass struct{ Pos }

// break statement
type Break struct{ Pos }

// continue statement
type Continue struct{ Pos }

// Chain of and or or operands
type BoolOp struct {
	Pos
	Op     BoolOperator
	Values []Expr
}

// Assignment expression with the walrus operator
type NamedExpr struct {
	Pos
	Target Expr
	Value  Expr
}

// Binary operation
type BinOp struct {
	Pos
	Left  Expr
	Op    BinaryOperator
	Right Expr
}

// Unary operation
type UnaryOp struct {
	Pos
	Op      UnaryOperator
	Operand Expr
}

// lambda expression
type Lambda struct {
	Pos
	Args *Arguments
	Body Expr
}

// Conditional expression body if test else orelse
type IfExp struct {
	Pos
	Test   Expr
	Body   Expr
	Orelse Expr
}

// dict display, a nil key marking a double starred unpacking of its value
type Dict struct {
	Pos
	Keys   []Expr
	Values []Expr
}

// set display
type Set struct {
	Pos
	Elts []Expr
}

// list comprehension
type ListComp struct {
	Pos
	Elt        Expr
	Generators []*Comprehension
}

// set comprehension
type SetComp struct {
	Pos
	Elt        Expr
	Generators []*Comprehension
}

// dict comprehension
type DictComp struct {
	Pos
	Key        Expr
	Value      Expr
	Generators []*Comprehension
}

// Generator expression
type GeneratorExp struct {
	Pos
	Elt        Expr
	Generators []*Comprehension
}

// await expression
type Await struct {
	Pos
	Value Expr
}

// yield expression with an optional value
type Yield struct {
	Pos
	Value Expr
}

// yield from expression
type YieldFrom struct {
	Pos
	Value Expr
}

// Comparison chain, one operator per comparator
type Compare struct {
	Pos
	Left        Expr
	Ops         []CmpOperator
	Comparators []Expr
}

// Function call with positional and starred arguments in Args and keyword arguments in Keywords
type Call struct {
	Pos
	Func     Expr
	Args     []Expr
	Keywords []*Keyword
}

// One replacement field of an f-string, Conversion -1 or the code of s, r or a
type FormattedValue struct {
	Pos
	Value      Expr
	Conversion int
	FormatSpec Expr
}

// One replacement field of a t-string with its expression text
type Interpolation struct {
	Pos
	Value      Expr
	Str        string
	Conversion int
	FormatSpec Expr
}

// F-string or a format spec, its parts constants and formatted values
type JoinedStr struct {
	Pos
	Values []Expr
}

// T-string, its parts constants and interpolations
type TemplateStr struct {
	Pos
	Values []Expr
}

// Literal, Kind u for a string written with the u prefix
type Constant struct {
	Pos
	Value Value
	Kind  string
}

// attribute access value.attr
type Attribute struct {
	Pos
	Value Expr
	Attr  string
	Ctx   Context
}

// Subscription value[slice]
type Subscript struct {
	Pos
	Value Expr
	Slice Expr
	Ctx   Context
}

// starred expression
type Starred struct {
	Pos
	Value Expr
	Ctx   Context
}

// Identifier, normalized with NFKC when it holds non ASCII characters
type Name struct {
	Pos
	ID  string
	Ctx Context
}

// list display or list target
type List struct {
	Pos
	Elts []Expr
	Ctx  Context
}

// tuple display or tuple target
type Tuple struct {
	Pos
	Elts []Expr
	Ctx  Context
}

// slice inside a subscript with optional lower, upper and step
type Slice struct {
	Pos
	Lower Expr
	Upper Expr
	Step  Expr
}

// Pattern matching a value compared by equality
type MatchValue struct {
	Pos
	Value Expr
}

// Pattern matching None, True or False by identity
type MatchSingleton struct {
	Pos
	Value Value
}

// Sequence pattern
type MatchSequence struct {
	Pos
	Patterns []Pattern
}

// Mapping pattern, Rest naming the double starred capture or empty
type MatchMapping struct {
	Pos
	Keys     []Expr
	Patterns []Pattern
	Rest     string
}

// Class pattern with positional and keyword sub patterns
type MatchClass struct {
	Pos
	Cls         Expr
	Patterns    []Pattern
	KwdAttrs    []string
	KwdPatterns []Pattern
}

// Starred rest of a sequence pattern, Name empty for the wildcard
type MatchStar struct {
	Pos
	Name string
}

// Capture or wildcard pattern, optionally binding a sub pattern
type MatchAs struct {
	Pos
	Pattern Pattern
	Name    string
}

// Pattern matching any of its alternatives
type MatchOr struct {
	Pos
	Patterns []Pattern
}

// One for clause of a comprehension with its if filters
type Comprehension struct {
	Target Expr
	Iter   Expr
	Ifs    []Expr
	Async  bool
}

// Except or except* clause, Name empty without an as binding
type ExceptHandler struct {
	Pos
	Type Expr
	Name string
	Body []Stmt
}

// Parameter list, KwDefaults holding nil for keyword only parameters without a default
type Arguments struct {
	PosOnly    []*Arg
	Args       []*Arg
	Vararg     *Arg
	KwOnly     []*Arg
	KwDefaults []Expr
	Kwarg      *Arg
	Defaults   []Expr
}

// One parameter with an optional annotation
type Arg struct {
	Pos
	Name       string
	Annotation Expr
}

// keyword argument, Arg empty for a double starred unpacking
type Keyword struct {
	Pos
	Arg   string
	Value Expr
}

// One imported name with an optional as name
type Alias struct {
	Pos
	Name   string
	Asname string
}

// One context manager of a with statement
type WithItem struct {
	ContextExpr  Expr
	OptionalVars Expr
}

// One case clause of a match statement
type MatchCase struct {
	Pattern Pattern
	Guard   Expr
	Body    []Stmt
}

// Type variable parameter with optional bound and default
type TypeVar struct {
	Pos
	Name    string
	Bound   Expr
	Default Expr
}

// Double starred type parameter
type ParamSpec struct {
	Pos
	Name    string
	Default Expr
}

// starred type parameter
type TypeVarTuple struct {
	Pos
	Name    string
	Default Expr
}

func (*Module) node()         {}
func (*FunctionDef) node()    {}
func (*ClassDef) node()       {}
func (*Return) node()         {}
func (*Delete) node()         {}
func (*Assign) node()         {}
func (*TypeAlias) node()      {}
func (*AugAssign) node()      {}
func (*AnnAssign) node()      {}
func (*For) node()            {}
func (*While) node()          {}
func (*If) node()             {}
func (*With) node()           {}
func (*Match) node()          {}
func (*Raise) node()          {}
func (*Try) node()            {}
func (*Assert) node()         {}
func (*Import) node()         {}
func (*ImportFrom) node()     {}
func (*Global) node()         {}
func (*Nonlocal) node()       {}
func (*ExprStmt) node()       {}
func (*Pass) node()           {}
func (*Break) node()          {}
func (*Continue) node()       {}
func (*BoolOp) node()         {}
func (*NamedExpr) node()      {}
func (*BinOp) node()          {}
func (*UnaryOp) node()        {}
func (*Lambda) node()         {}
func (*IfExp) node()          {}
func (*Dict) node()           {}
func (*Set) node()            {}
func (*ListComp) node()       {}
func (*SetComp) node()        {}
func (*DictComp) node()       {}
func (*GeneratorExp) node()   {}
func (*Await) node()          {}
func (*Yield) node()          {}
func (*YieldFrom) node()      {}
func (*Compare) node()        {}
func (*Call) node()           {}
func (*FormattedValue) node() {}
func (*Interpolation) node()  {}
func (*JoinedStr) node()      {}
func (*TemplateStr) node()    {}
func (*Constant) node()       {}
func (*Attribute) node()      {}
func (*Subscript) node()      {}
func (*Starred) node()        {}
func (*Name) node()           {}
func (*List) node()           {}
func (*Tuple) node()          {}
func (*Slice) node()          {}
func (*MatchValue) node()     {}
func (*MatchSingleton) node() {}
func (*MatchSequence) node()  {}
func (*MatchMapping) node()   {}
func (*MatchClass) node()     {}
func (*MatchStar) node()      {}
func (*MatchAs) node()        {}
func (*MatchOr) node()        {}
func (*Comprehension) node()  {}
func (*ExceptHandler) node()  {}
func (*Arguments) node()      {}
func (*Arg) node()            {}
func (*Keyword) node()        {}
func (*Alias) node()          {}
func (*WithItem) node()       {}
func (*MatchCase) node()      {}
func (*TypeVar) node()        {}
func (*ParamSpec) node()      {}
func (*TypeVarTuple) node()   {}

func (*FunctionDef) stmt() {}
func (*ClassDef) stmt()    {}
func (*Return) stmt()      {}
func (*Delete) stmt()      {}
func (*Assign) stmt()      {}
func (*TypeAlias) stmt()   {}
func (*AugAssign) stmt()   {}
func (*AnnAssign) stmt()   {}
func (*For) stmt()         {}
func (*While) stmt()       {}
func (*If) stmt()          {}
func (*With) stmt()        {}
func (*Match) stmt()       {}
func (*Raise) stmt()       {}
func (*Try) stmt()         {}
func (*Assert) stmt()      {}
func (*Import) stmt()      {}
func (*ImportFrom) stmt()  {}
func (*Global) stmt()      {}
func (*Nonlocal) stmt()    {}
func (*ExprStmt) stmt()    {}
func (*Pass) stmt()        {}
func (*Break) stmt()       {}
func (*Continue) stmt()    {}

func (*BoolOp) expr()         {}
func (*NamedExpr) expr()      {}
func (*BinOp) expr()          {}
func (*UnaryOp) expr()        {}
func (*Lambda) expr()         {}
func (*IfExp) expr()          {}
func (*Dict) expr()           {}
func (*Set) expr()            {}
func (*ListComp) expr()       {}
func (*SetComp) expr()        {}
func (*DictComp) expr()       {}
func (*GeneratorExp) expr()   {}
func (*Await) expr()          {}
func (*Yield) expr()          {}
func (*YieldFrom) expr()      {}
func (*Compare) expr()        {}
func (*Call) expr()           {}
func (*FormattedValue) expr() {}
func (*Interpolation) expr()  {}
func (*JoinedStr) expr()      {}
func (*TemplateStr) expr()    {}
func (*Constant) expr()       {}
func (*Attribute) expr()      {}
func (*Subscript) expr()      {}
func (*Starred) expr()        {}
func (*Name) expr()           {}
func (*List) expr()           {}
func (*Tuple) expr()          {}
func (*Slice) expr()          {}

func (*MatchValue) pattern()     {}
func (*MatchSingleton) pattern() {}
func (*MatchSequence) pattern()  {}
func (*MatchMapping) pattern()   {}
func (*MatchClass) pattern()     {}
func (*MatchStar) pattern()      {}
func (*MatchAs) pattern()        {}
func (*MatchOr) pattern()        {}

func (*TypeVar) typeParam()      {}
func (*ParamSpec) typeParam()    {}
func (*TypeVarTuple) typeParam() {}
