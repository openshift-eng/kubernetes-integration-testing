package cmd

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
)

var follow bool
var lines int

var logsCmd = &cobra.Command{
	Use:   "logs",
	Short: "Show daemon logs",
	RunE: func(cmd *cobra.Command, args []string) error {
		path := appStore.LogPath()

		f, err := os.Open(path)
		if err != nil {
			if os.IsNotExist(err) {
				return fmt.Errorf("no logs found (daemon never started?)")
			}
			return err
		}
		defer f.Close()

		if follow {
			return tailFollow(f)
		}
		return tailLines(f, lines)
	},
}

func init() {
	logsCmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	logsCmd.Flags().IntVarP(&lines, "lines", "n", 50, "number of lines to show")
	rootCmd.AddCommand(logsCmd)
}

func printLine(line string) {
	fmt.Println(line)
}

func tailLines(f *os.File, n int) error {
	scanner := bufio.NewScanner(f)
	var buf []string
	for scanner.Scan() {
		buf = append(buf, scanner.Text())
		if len(buf) > n {
			buf = buf[1:]
		}
	}
	for _, line := range buf {
		printLine(line)
	}
	return scanner.Err()
}

func tailFollow(f *os.File) error {
	f.Seek(0, io.SeekEnd)

	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			if err == io.EOF {
				continue
			}
			return err
		}
		printLine(strings.TrimRight(line, "\n"))
	}
}
