package main

import (
	"fmt"
)

func main() {

	nDao, err := ConstructNotionDaoFromEnv()
	if err != nil {
		panic(fmt.Errorf("configuration error: %w", err))
	}
    nDao.globalHash = map[string]string{}
	//get hash
	_ = GetHashMap(nDao)
	tasks := GetAllTasks()
	errs := make([]error, len(tasks))
	for i, t := range tasks {
		errs[i] = t.Run(nDao)
	}

	PanicOnErrors(errs)
}
