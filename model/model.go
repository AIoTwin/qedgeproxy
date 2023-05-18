package model

import "time"

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

type PodInfoCache struct {
	CacheTime   time.Time
	Pods        []*PodInfo
	Annotations map[string]string
}
