package api

import (
	"context"
	"errors"

	"pulse/internal/apigen"
)

// This file holds the StrictServerInterface methods that fall OUTSIDE the wired
// slices (last-failure). They exist so *Server satisfies the full generated
// interface and can be handed to apigen.NewStrictHandler, but no route maps to
// them yet. They are a later work package. Each returns errNotImplemented; the
// strict handler would surface it as a 500, but no route reaches them.
//
// Keeping them here (rather than scattering "TODO" handlers) makes the boundary of
// this work package explicit: everything in the other api files is real and wired;
// everything here is a deliberate not-yet.

// errNotImplemented marks an operation that is out of the wired slices.
var errNotImplemented = errors.New("not implemented yet")

// compile-time check that *Server implements the whole generated interface.
var _ apigen.StrictServerInterface = (*Server)(nil)

func (s *Server) GetLastFailure(context.Context, apigen.GetLastFailureRequestObject) (apigen.GetLastFailureResponseObject, error) {
	return nil, errNotImplemented
}
