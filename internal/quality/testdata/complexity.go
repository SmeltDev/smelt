package fixture

import "fmt"

type T struct{}

type G[K comparable] struct{ m map[K]int }

func empty() {}

func ifs(a, b int) int {
	if a > 0 {
		return 1
	} else if b > 0 {
		return 2
	}
	return 3
}

func loops(xs []int, ch chan int) int {
	n := 0
	for i := 0; i < 3; i++ {
		n += i
	}
	for range xs {
		n++
	}
	for _, x := range xs {
		n += x
	}
	for n < 100 {
		n *= 2
	}
	return n
}

func switches(v any, n int) string {
	switch n {
	case 1, 2:
		return "a"
	case 3:
		return "b"
	default:
		return "c"
	}
}

func typeSwitch(v any) string {
	switch v.(type) {
	case int:
		return "int"
	case string, bool:
		return "text"
	default:
		return "other"
	}
}

func selects(a, b chan int) int {
	select {
	case x := <-a:
		return x
	case y := <-b:
		return y
	default:
		return 0
	}
}

func logic(a, b, c bool) bool {
	return a && b || c
}

func closure(xs []int) func() int {
	return func() int {
		for _, x := range xs {
			if x > 0 && x < 10 {
				return x
			}
		}
		return 0
	}
}

func (t *T) method(a int) string {
	if a == 0 {
		return ""
	}
	return fmt.Sprint(a)
}

func (g G[K]) get(k K) int {
	if v, ok := g.m[k]; ok {
		return v
	}
	return 0
}

func init() {}

func init() {
	if len(fmt.Sprint(1)) > 0 {
		return
	}
}
