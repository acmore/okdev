package connect

// DeliveryError means command completion is unknown. Retrying could run a
// mutation twice, even if the underlying failure looks transient.
type DeliveryError struct{ Err error }

func (e *DeliveryError) Error() string {
	return "command transport failed; completion unknown (not replayed): " + e.Err.Error()
}
func (e *DeliveryError) Unwrap() error { return e.Err }
