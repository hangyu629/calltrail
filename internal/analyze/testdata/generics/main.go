package main

type Number interface{ ~int | ~float64 }

// Sum is instantiated twice; both instances must fold into one node.
func Sum[T Number](xs []T) T {
	var s T
	for _, x := range xs {
		s += x
	}
	return s
}

type Stack[T any] struct{ items []T }

func (s *Stack[T]) Push(x T) { s.items = append(s.items, x) }
func (s *Stack[T]) Len() int { return len(s.items) }
func (s *Stack[T]) marker()  {} // never called, implements nothing

type Lener interface{ Len() int }

// Container is a generic interface; Stack[int] implements Container[int].
type Container[T any] interface {
	Push(T)
	Len() int
}

type Set[K comparable] map[K]struct{}

func (s Set[K]) Len() int { return len(s) }
func (s Set[K]) Add(k K)  { s[k] = struct{}{} }

// sealed is satisfied only by a method on a generic type.
type sealed interface{ seal() }
type A[T any] struct{}

func (A[T]) seal() {}

var _ sealed = A[int]{}

func use(c Container[int]) int {
	c.Push(1)
	return c.Len()
}

func Map[T, U any](xs []T, f func(T) U) []U {
	var out []U
	for _, x := range xs {
		out = append(out, f(x))
	}
	return out
}

func main() {
	st := &Stack[int]{}
	println(use(st))
	println(Sum([]int{1, 2}), Sum([]float64{1.5}))
	_ = Map([]int{1}, func(i int) string { return "x" })
	var l Lener = Set[string]{}
	println(l.Len())
}

// Never instantiated anywhere: its methods exist only as generic origins.
type Orphan[T any] struct{}

func (Orphan[T]) Hello() {}

// Orphan's method calls plain helpers: no instance exists, so the edges can only come from
// the generic origin's own body.
func (Orphan[T]) World() { orphanHelper() }

func orphanHelper() {}
