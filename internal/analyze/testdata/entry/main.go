package main

import "example.com/entry/lib"

func main() { run() }

func run() { lib.Used() }

// never called from main
func unreachable() { lib.Dead() }
