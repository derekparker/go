// prethread demonstrates a thread that predates Go runtime initialization
// (started from a C constructor) touching the Go heap, and shows which
// memory-policy layer covers it.
package main

/*
#cgo LDFLAGS: -lpthread
int taskPolicyMode(void);
long tid(void);
void markReady(void);
void joinPrethread(void);
*/
import "C"

import (
	"bufio"
	"fmt"
	"os"
	"runtime"
	"strings"
)

var sink [][]byte

func report(who string) {
	mode := int(C.taskPolicyMode())
	name := map[int]string{0: "MPOL_DEFAULT", 1: "MPOL_PREFERRED", 2: "MPOL_BIND", 3: "MPOL_INTERLEAVE", 4: "MPOL_LOCAL"}[mode]
	fmt.Printf("%-28s tid=%-8d task mempolicy mode=%d (%s)\n", who, int(C.tid()), mode, name)
}

//export goWork
func goWork() {
	runtime.LockOSThread()
	report("C-constructor thread in Go:")
	// Touch fresh Go heap pages from this thread.
	for i := 0; i < 64; i++ {
		b := make([]byte, 1<<20)
		for j := 0; j < len(b); j += 4096 {
			b[j] = 1
		}
		sink = append(sink, b)
	}
	fmt.Printf("%-28s allocated and touched %d MiB of Go heap\n", "C-constructor thread in Go:", len(sink))
}

func main() {
	runtime.LockOSThread()
	report("Go main thread:")
	C.markReady()
	C.joinPrethread()
	// Show the VMA policies the heap ranges carry.
	f, err := os.Open("/proc/self/numa_maps")
	if err != nil {
		fmt.Println("numa_maps:", err)
		return
	}
	defer f.Close()
	counts := map[string]int{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 2 {
			continue
		}
		// Only the Go runtime installs VMA memory policies in this
		// process; every range carrying one is a heap range.
		if strings.HasPrefix(fields[1], "bind:") || strings.HasPrefix(fields[1], "prefer:") {
			counts[fields[1]]++
		}
	}
	fmt.Printf("%-28s VMA policies on Go heap ranges (policy: count): %v\n", "numa_maps:", counts)
}
