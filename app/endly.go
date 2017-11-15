package main

import (
	"flag"

	"github.com/viant/endly"
	"log"
	_ "github.com/go-sql-driver/mysql"
	_ "github.com/viant/asc"
	_ "github.com/viant/bgc"
	_ "github.com/alexbrainman/odbc"
	"time"
)

var workflow = flag.String("workflow", "run.json", "path to workflow run request json file")

func main() {
	flag.Parse()
	runner := endly.NewCliRunner()
	err := runner.Run(*workflow)
	if err != nil {
		log.Fatal(err)
	}
	time.Sleep(time.Second)
}
