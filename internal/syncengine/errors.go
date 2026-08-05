package syncengine

import "errors"

var (
	ErrInvalidResourceKey      = errors.New("invalid resource key")
	ErrUnknownResourceType     = errors.New("unknown resource type")
	ErrProviderNotRegistered   = errors.New("resource provider is not registered")
	ErrDuplicateProvider       = errors.New("resource provider is already registered")
	ErrInvalidOpenedResource   = errors.New("invalid opened resource")
	ErrInvalidSnapshot         = errors.New("invalid snapshot")
	ErrInvalidChange           = errors.New("invalid resource change")
	ErrJournalClosed           = errors.New("journal is closed")
	ErrChangeTooLarge          = errors.New("resource change exceeds journal byte limit")
	ErrSequenceExhausted       = errors.New("stream sequence is exhausted")
	ErrEpochUnchanged          = errors.New("stream epoch did not change")
	ErrInvalidEpoch            = errors.New("invalid stream epoch")
	ErrInvalidDeliveryCapacity = errors.New("invalid live delivery capacity")
	ErrSentEpochMismatch       = errors.New("sent sequence has a different stream epoch")
	ErrSentRegression          = errors.New("sent sequence moved backwards")
	ErrAckEpochMismatch        = errors.New("ack has a different stream epoch")
	ErrAckAhead                = errors.New("ack is ahead of the last sent sequence")
	ErrAckRegression           = errors.New("ack moved backwards")
)
