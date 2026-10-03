//go:build center

package main

import (
	"flag"
	"fmt"
	"log"
)

func main() {
	role := flag.String("role", "center", "兼容旧命令行（恒为 center，忽略）")
	conf := flag.String("conf", "", "config file path")
	listen := flag.String("listen", "", "listen addr (center override)")
	dbDSN := flag.String("db", "", "postgres dsn (center override)")
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	fmt.Printf("pingatlas-center %s (role=%s)\n", Version, *role)
	runCenter(*conf, *listen, *dbDSN)
}
