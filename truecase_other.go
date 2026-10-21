//go:build !darwin

package main

func diskPath(p string) (string, error) { return p, nil }
