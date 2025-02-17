package mutate

import (
	"context"

	"github.com/seventv/api/data/events"
	"github.com/seventv/common/errors"
	"github.com/seventv/common/mongo"
	"github.com/seventv/common/structures/v3"
	"github.com/seventv/common/utils"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.uber.org/zap"
)

func (m *Mutate) SetUserConnectionActiveEmoteSet(ctx context.Context, ub *structures.UserBuilder, opt SetUserActiveEmoteSet) error {
	if ub == nil {
		return errors.ErrInternalIncompleteMutation()
	} else if ub.IsTainted() {
		return errors.ErrMutateTaintedObject()
	}

	// Check for actor's permission to do this
	actor := opt.Actor
	victim := &ub.User

	oldSet := opt.OldSet
	newSet := opt.NewSet

	if !opt.SkipValidation {
		if actor.ID != victim.ID { // actor is modfiying another user
			notPrivileged := errors.ErrInsufficientPrivilege().SetDetail("You are not allowed to change the active Emote Set of this user")

			if !actor.HasPermission(structures.RolePermissionManageUsers) { // actor is not a moderator
				ed, ok, _ := victim.GetEditor(actor.ID)
				if !ok { // actor is not an editor of the victim
					return notPrivileged
				}

				if !ed.HasPermission(structures.UserEditorPermissionManageEmoteSets) { // actor lacks the necessary permission
					return notPrivileged
				}
			}
		}

		// Validate that the emote set exists and can be enabled
		if !newSet.ID.IsZero() {
			if !actor.HasPermission(structures.RolePermissionEditAnyEmoteSet) && newSet.OwnerID != victim.ID {
				return errors.ErrInsufficientPrivilege().
					SetFields(errors.Fields{"owner_id": newSet.OwnerID.Hex()}).
					SetDetail("You cannot assign another user's Emote Set to your channel")
			}
		}
	}

	// Get the connection
	conn, connInd := ub.GetConnection("", opt.ConnectionID)
	if conn == nil || connInd < 0 {
		return errors.ErrUnknownUserConnection()
	}

	conn.SetActiveEmoteSet(newSet.ID)

	// Update document
	if err := m.mongo.Collection(mongo.CollectionNameUsers).FindOneAndUpdate(
		ctx,
		bson.M{
			"_id":            victim.ID,
			"username":       victim.Username,
			"connections.id": opt.ConnectionID,
		},
		conn.Update,
		options.FindOneAndUpdate().SetReturnDocument(options.After),
	).Decode(victim); err != nil {
		if err == mongo.ErrNoDocuments {
			return errors.ErrUnknownUser().SetDetail("Victim was not found and could not be updated")
		}

		zap.S().Errorw("failed to update user", "error", err)

		return errors.ErrInternalServerError().SetDetail(err.Error())
	}

	// Emit event about the user's set switching
	func() {
		newSet.Emotes = nil // don't send the emotes with the event
		oldSet.Emotes = nil

		m.events.Dispatch(ctx, events.EventTypeUpdateUser, events.ChangeMap{
			ID:    ub.User.ID,
			Kind:  structures.ObjectKindUser,
			Actor: m.modelizer.User(actor).ToPartial(),
			Updated: []events.ChangeField{
				{
					Key:    "connections",
					Index:  utils.PointerOf(int32(connInd)),
					Nested: true,
					Value: []events.ChangeField{
						{
							Key:      "emote_set",
							Type:     events.ChangeFieldTypeObject,
							OldValue: utils.Ternary(oldSet.ID.IsZero(), nil, utils.PointerOf(m.modelizer.EmoteSet(oldSet))),
							Value:    utils.Ternary(newSet.ID.IsZero(), nil, utils.PointerOf(m.modelizer.EmoteSet(newSet))),
						},
						{
							Key:      "emote_set_id",
							Type:     events.ChangeFieldTypeString,
							OldValue: utils.Ternary(oldSet.ID.IsZero(), nil, utils.PointerOf(oldSet.ID.Hex())),
							Value:    utils.Ternary(newSet.ID.IsZero(), nil, utils.PointerOf(newSet.ID.Hex())),
						},
					},
				},
			},
		}, events.EventCondition{
			"object_id": ub.User.ID.Hex(),
		})
	}()

	ub.MarkAsTainted()

	return nil
}

type SetUserActiveEmoteSet struct {
	NewSet         structures.EmoteSet
	OldSet         structures.EmoteSet
	Platform       structures.UserConnectionPlatform
	Actor          structures.User
	ConnectionID   string
	SkipValidation bool
}
