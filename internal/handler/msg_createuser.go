// Copyright 2021 FerretDB Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package handler

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/FerretDB/FerretDB/internal/backends"
	"github.com/FerretDB/FerretDB/internal/handler/common"
	"github.com/FerretDB/FerretDB/internal/handler/handlererrors"
	"github.com/FerretDB/FerretDB/internal/handler/handlerparams"
	"github.com/FerretDB/FerretDB/internal/types"
	"github.com/FerretDB/FerretDB/internal/util/iterator"
	"github.com/FerretDB/FerretDB/internal/util/lazyerrors"
	"github.com/FerretDB/FerretDB/internal/util/must"
	"github.com/FerretDB/FerretDB/internal/util/password"
	"github.com/FerretDB/FerretDB/internal/wire"
)

// MsgCreateUser implements `createUser` command.
func (h *Handler) MsgCreateUser(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := msg.Document()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	if dbName != "$external" && !document.Has("pwd") {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrBadValue,
			"Must provide a 'pwd' field for all user documents, except those with '$external' as the user's source db",
		)
	}

	username, err := common.GetRequiredParam[string](document, document.Command())
	if err != nil {
		return nil, err
	}

	if username == "" {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrBadValue,
			"User document needs 'user' field to be non-empty",
		)
	}

	if err = common.UnimplementedNonDefault(document, "customData", func(v any) bool {
		if v == nil || v == types.Null {
			return true
		}

		cd, ok := v.(*types.Document)
		return ok && cd.Len() == 0
	}); err != nil {
		return nil, err
	}

	if _, err = common.GetRequiredParam[*types.Array](document, "roles"); err != nil {
		var ce *handlererrors.CommandError
		if errors.As(err, &ce) && ce.Code() == handlererrors.ErrBadValue {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrMissingField,
				"BSON field 'createUser.roles' is missing but a required field",
			)
		}

		return nil, lazyerrors.Error(err)
	}

	if err = common.UnimplementedNonDefault(document, "roles", func(v any) bool {
		r, ok := v.(*types.Array)
		return ok && r.Len() == 0
	}); err != nil {
		return nil, err
	}

	if err = common.UnimplementedNonDefault(document, "digestPassword", func(v any) bool {
		if v == nil || v == types.Null {
			return true
		}

		dp, ok := v.(bool)
		return ok && dp
	}); err != nil {
		return nil, err
	}

	common.Ignored(document, h.L, "writeConcern", "authenticationRestrictions", "comment")

	defMechanisms := must.NotFail(types.NewArray("SCRAM-SHA-1", "SCRAM-SHA-256"))

	mechanisms, err := common.GetOptionalParam(document, "mechanisms", defMechanisms)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if mechanisms.Len() == 0 {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrBadValue,
			"mechanisms field must not be empty",
		)
	}

	iter := mechanisms.Iterator()
	defer iter.Close()

	for {
		var v any
		_, v, err := iter.Next()

		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		switch v {
		case "SCRAM-SHA-1", "SCRAM-SHA-256":
			// do nothing
		default:
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrBadValue,
				fmt.Sprintf("Unknown auth mechanism '%s'", v),
			)
		}
	}

	if document.Has("pwd") {
		pwd, _ := document.Get("pwd")
		userPassword, ok := pwd.(string)

		if !ok {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrTypeMismatch,
				fmt.Sprintf("BSON field 'createUser.pwd' is the wrong type '%s', expected type 'string'",
					handlerparams.AliasFromType(pwd),
				),
			)
		}

		if userPassword == "" {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrSetEmptyPassword,
				"Password cannot be empty",
			)
		}

		err = backends.CreateUser(ctx, h.b, &backends.CreateUserParams{
			Database:   dbName,
			Username:   username,
			Password:   password.WrapPassword(userPassword),
			Mechanisms: mechanisms,
		})
		if err != nil {
			if backends.ErrorCodeIs(err, backends.ErrorCodeInsertDuplicateID) {
				return nil, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrUserAlreadyExists,
					fmt.Sprintf("User \"%s@%s\" already exists", username, dbName),
				)
			}

			if strings.Contains(err.Error(), "prohibited character") {
				return nil, handlererrors.NewCommandErrorMsg(
					handlererrors.ErrStringProhibited,
					"Error preflighting normalization: U_STRINGPREP_PROHIBITED_ERROR",
				)
			}

			return nil, lazyerrors.Error(err)
		}
	}

	var reply wire.OpMsg
	must.NoError(reply.SetSections(wire.MakeOpMsgSection(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)))

	return &reply, nil
}
