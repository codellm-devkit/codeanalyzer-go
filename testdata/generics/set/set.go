package set

// Set is a generic hash set.
type Set[T comparable] struct {
	items map[T]struct{}
}

func New[T comparable]() *Set[T] {
	return &Set[T]{items: make(map[T]struct{})}
}

func (s *Set[T]) Add(v T) {
	s.items[v] = struct{}{}
}

func (s *Set[T]) Remove(v T) {
	delete(s.items, v)
}

func (s *Set[T]) Contains(v T) bool {
	_, ok := s.items[v]
	return ok
}

func (s *Set[T]) Len() int {
	return len(s.items)
}

// Snapshot returns all elements as a slice. Unexported helper for internal use.
func (s *Set[T]) snapshot() []T {
	out := make([]T, 0, len(s.items))
	for k := range s.items {
		out = append(out, k)
	}
	return out
}
