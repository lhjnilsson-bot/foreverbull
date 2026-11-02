package usercopy

// Answers of the service commands this version doesn't implement.
const (
	ServiceStatusUnimplemented = "This version of foreverbull can't report a strategy service's status. " + ReportIt
	InterviewUnimplemented     = "This version of foreverbull can't ask a strategy worker for its setup when it starts. " + ReportIt
	SanityCheckUnimplemented   = "This version of foreverbull can't check that a strategy worker is ready when it starts. " + ReportIt
	InstanceStopUnimplemented  = "This version of foreverbull can't stop a strategy worker. " + ReportIt
)
