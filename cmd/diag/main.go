package main

import (
	"fmt"
	"log"
	"time"

	"github.com/goburrow/modbus"
)

func main() {
	h502 := modbus.NewTCPClientHandler("192.168.86.86:502")
	h502.Timeout = 3 * time.Second
	if err := h502.Connect(); err != nil {
		log.Fatalf("connect 502: %v", err)
	}
	defer h502.Close()
	c502 := modbus.NewClient(h502)

	fmt.Println("=== Idling all inverters ===")
	for _, uid := range []byte{11, 12, 13, 14} {
		h502.SlaveId = uid
		_, err := c502.WriteSingleRegister(40213, 0)
		if err != nil {
			fmt.Printf("  Unit %d idle: ERR %v\n", uid, err)
		} else {
			fmt.Printf("  Unit %d idle: OK\n", uid)
		}
		time.Sleep(2 * time.Second)
	}
}
