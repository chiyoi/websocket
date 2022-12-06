package logs

import (
	"log"
	"os"
)

var (
	lError = log.New(os.Stderr, "[ERROR] ", log.LstdFlags|log.Lshortfile|log.Lmsgprefix)
	lInfo  = log.New(os.Stdout, "[INFO] ", log.LstdFlags|log.Lshortfile|log.Lmsgprefix)
	lDebug = log.New(os.Stdout, "[DEBUG] ", log.LstdFlags|log.Lshortfile|log.Lmsgprefix)
)

var (
	Error = lError.Println
	Info  = lInfo.Println
	Debug = lDebug.Println
)
