package main

import (
	"encoding/json"
	"fmt"
)

// Color is only reachable as a field of Wrapper: encoding/json finds MarshalJSON by reflection.
type Color struct{}

func (Color) MarshalJSON() ([]byte, error) { return []byte(`"c"`), nil }

type Wrapper struct {
	C     Color
	Items []*Color
	skip  Color // unexported: json never calls methods on it
}

func encode(w Wrapper) {
	b, _ := json.Marshal(w) // w -> any
	fmt.Println(string(b))
}

// Shape is an own interface; show converts a Shape to any (ChangeInterface), fmt may call String
// on whatever it holds.
type Shape interface{ Area() float64 }

type Sq struct{}

func (Sq) Area() float64  { return 1 }
func (Sq) String() string { return "sq" }

func show(s Shape) { fmt.Println(s) }

// Four more places that hand a Wrapper to encoding/json: Color.MarshalJSON then has five implicit
// callers, but only maxImplicitEdges of them are drawn.
func encodeB(w Wrapper) { b, _ := json.Marshal(w); fmt.Println(b) }
func encodeC(w Wrapper) { b, _ := json.Marshal(w); fmt.Println(b) }
func encodeD(w Wrapper) { b, _ := json.Marshal(w); fmt.Println(b) }
func encodeE(w Wrapper) { b, _ := json.Marshal(w); fmt.Println(b) }

// Sq2.String is called by name as well, so the implicit edge from showSq2 is not drawn at all.
type Sq2 struct{}

func (Sq2) String() string { return "sq2" }

func callExplicit(s Sq2) string { return s.String() }
func showSq2(s Sq2)             { fmt.Println(s) }

// Doc holds an interface-typed field: json walks into whatever it holds at run time.
type Item interface{ Kind() string }

type Text struct{}

func (Text) Kind() string                 { return "t" }
func (Text) MarshalJSON() ([]byte, error) { return []byte(`"t"`), nil }

type Doc struct{ Items []Item }

func build() Doc { return Doc{Items: []Item{Text{}}} }

func encodeDoc(d Doc) { b, _ := json.Marshal(d); fmt.Println(string(b)) }

// errors.Is calls Is on an error value.
type MyErr struct{}

func (MyErr) Error() string        { return "e" }
func (MyErr) Is(target error) bool { return true }

func fail() error { return MyErr{} }

func main() {
	encode(Wrapper{})
	encodeB(Wrapper{})
	encodeC(Wrapper{})
	encodeD(Wrapper{})
	encodeE(Wrapper{})
	show(Sq{})
	_ = callExplicit(Sq2{})
	showSq2(Sq2{})
	encodeDoc(build())
	_ = fail()
}
