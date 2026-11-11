package state

// Whether a model_config key is one of the constructor facts a worker may add to its reserved row
func IsWorkerConstructorConfigKey(key string) bool { return workerConstructorConfig[key] }
