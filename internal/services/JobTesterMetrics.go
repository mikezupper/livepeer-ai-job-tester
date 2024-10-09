package services

import "sync"

// JobTesterMetrics stores the metrics related to job testing results.
// It tracks the total number of jobs, jobs that passed, jobs that failed,
// jobs with tester errors, and the expected total number of jobs.
// A read-write mutex is used to safely handle concurrent updates.
type JobTesterMetrics struct {
	lock sync.RWMutex // RWMutex ensures safe concurrent access to the fields.

	TotalJobs            int `json:"total_jobs"`              // Total number of jobs processed.
	TotalJobsTesterError int `json:"total_jobs_tester_error"` // Number of jobs that encountered tester errors.
	TotalJobsPassed      int `json:"total_jobs_passed"`       // Number of jobs that passed successfully.
	TotalJobsFailed      int `json:"total_jobs_failed"`       // Number of jobs that failed.
	ExpectedTotalJobs    int `json:"expected_total_jobs"`     // The expected number of jobs to process.
}

// NewJobTesterMetrics initializes and returns a pointer to a new JobTesterMetrics instance.
// The returned instance starts with all metrics initialized to zero.
func NewJobTesterMetrics() *JobTesterMetrics {
	return &JobTesterMetrics{}
}

// IncrementTotalJobs increments the count of TotalJobs by 1.
// This method locks the mutex to ensure thread-safe operation.
func (js *JobTesterMetrics) IncrementTotalJobs() {
	js.lock.Lock()
	defer js.lock.Unlock()
	js.TotalJobs++
}

// IncrementTotalJobsTesterError increments the count of TotalJobsTesterError by 1.
// This method locks the mutex to ensure thread-safe operation.
func (js *JobTesterMetrics) IncrementTotalJobsTesterError() {
	js.lock.Lock()
	defer js.lock.Unlock()
	js.TotalJobsTesterError++
}

// IncrementTotalJobsPassed increments the count of TotalJobsPassed by 1.
// This method locks the mutex to ensure thread-safe operation.
func (js *JobTesterMetrics) IncrementTotalJobsPassed() {
	js.lock.Lock()
	defer js.lock.Unlock()
	js.TotalJobsPassed++
}

// IncrementTotalJobsFailed increments the count of TotalJobsFailed by 1.
// This method locks the mutex to ensure thread-safe operation.
func (js *JobTesterMetrics) IncrementTotalJobsFailed() {
	js.lock.Lock()
	defer js.lock.Unlock()
	js.TotalJobsFailed++
}

// IncrementExpectedTotalJobs increments the count of ExpectedTotalJobs by 1.
// This method locks the mutex to ensure thread-safe operation.
func (js *JobTesterMetrics) IncrementExpectedTotalJobs() {
	js.lock.Lock()
	defer js.lock.Unlock()
	js.ExpectedTotalJobs++
}
