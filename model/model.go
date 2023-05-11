package model

type PodInfo struct {
	Namespace string
	Name      string
	IP        string
	HostIP    string
}

type LatencyInfo struct {
	AverageLatency int
	ReqCount       int
}

type BestPodInfo struct {
	IP     string
	HostIP string
}
