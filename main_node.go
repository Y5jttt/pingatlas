//go:build node

package main

import (
	"flag"
	"fmt"
	"log"
)

func main() {
	role := flag.String("role", "node", "兼容旧命令行（恒为 node，忽略）")
	conf := flag.String("conf", "", "config file path")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	fmt.Printf("pingatlas-node %s (role=%s)\n", Version, *role)
	runNode(*conf)
}
