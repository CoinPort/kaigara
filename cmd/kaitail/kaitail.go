package main

import (
	"flag"
	"fmt"

	"github.com/openware/kaigara/pkg/logstream"
	"github.com/openware/kaigara/pkg/utils"
)

var (
	channel  = flag.String("c", "log.*", "Redis channel pattern to subscribe")
	showName = flag.Bool("n", false, "Show channel name")
)

func main() {
	flag.Parse()

	ls := logstream.NewRedisClient(utils.GetEnv("KAIGARA_REDIS_URL", "redis://localhost:6379/0"))
	ch := ls.Subscribe(*channel)

	// The channel and payload are fields on the message. This used to
	// reconstruct them by regex-matching the message's formatted string,
	// which dropped every multi-line payload -- "." does not match newlines
	// -- and whose [A-z.] class also spanned the punctuation between the
	// upper- and lower-case ASCII ranges.
	for msg := range ch {
		if *showName {
			fmt.Printf("%s: %s\n", msg.Channel, msg.Payload)
		} else {
			fmt.Printf("%s\n", msg.Payload)
		}
	}
}
