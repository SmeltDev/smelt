package main

import "os"

func run() error { return nil }

func main() {
	run()
	os.Remove("x")
}
