package main

import (
	v1 "github.com/cyeinfpro/ModelGuard/internal/pluginv1"
	"github.com/cyeinfpro/ModelGuard/internal/runtime"
	"log"
)

func main() {
	p, err := runtime.New()
	if err != nil {
		log.Fatal("fingerprint initialization failed")
	}
	defer p.Close()
	v1.Serve(p)
}
