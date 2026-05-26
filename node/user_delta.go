package node

import (
	"sort"

	panel "github.com/wyx2685/v2node/api/v2board"
)

func applyUserDeltaEvents(oldUsers []panel.UserInfo, events []panel.UserDeltaEvent) ([]panel.UserInfo, bool) {
	if len(events) == 0 {
		return oldUsers, false
	}

	usersByUUID := make(map[string]panel.UserInfo, len(oldUsers))
	uuidsByUserID := make(map[int]map[string]struct{}, len(oldUsers))
	for _, user := range oldUsers {
		if user.Uuid == "" {
			continue
		}
		addUserToIndex(usersByUUID, uuidsByUserID, user)
	}

	changed := false
	for _, event := range events {
		userID := event.UserID
		if userID <= 0 && len(event.Users) > 0 {
			userID = event.Users[0].Id
		}

		switch event.Action {
		case panel.UserDeltaActionUpsert:
			if userID > 0 {
				deleteUserID(usersByUUID, uuidsByUserID, userID)
			}
			for _, user := range event.Users {
				if user.Uuid == "" {
					continue
				}
				addUserToIndex(usersByUUID, uuidsByUserID, user)
			}
			changed = true
		case panel.UserDeltaActionDelete:
			if userID > 0 {
				deleteUserID(usersByUUID, uuidsByUserID, userID)
				changed = true
				continue
			}
			for _, user := range event.Users {
				if user.Uuid == "" {
					continue
				}
				if deleteUserUUID(usersByUUID, uuidsByUserID, user.Uuid) {
					changed = true
				}
			}
		}
	}

	if !changed {
		return oldUsers, false
	}

	next := make([]panel.UserInfo, 0, len(usersByUUID))
	for _, user := range usersByUUID {
		next = append(next, user)
	}
	sort.Slice(next, func(i, j int) bool {
		if next[i].Id != next[j].Id {
			return next[i].Id < next[j].Id
		}
		return next[i].Uuid < next[j].Uuid
	})
	return next, true
}

func addUserToIndex(usersByUUID map[string]panel.UserInfo, uuidsByUserID map[int]map[string]struct{}, user panel.UserInfo) {
	if user.Uuid == "" {
		return
	}
	if oldUser, ok := usersByUUID[user.Uuid]; ok && oldUser.Id > 0 && oldUser.Id != user.Id {
		if oldUUIDs := uuidsByUserID[oldUser.Id]; oldUUIDs != nil {
			delete(oldUUIDs, user.Uuid)
			if len(oldUUIDs) == 0 {
				delete(uuidsByUserID, oldUser.Id)
			}
		}
	}

	usersByUUID[user.Uuid] = user
	if user.Id <= 0 {
		return
	}
	if uuidsByUserID[user.Id] == nil {
		uuidsByUserID[user.Id] = make(map[string]struct{})
	}
	uuidsByUserID[user.Id][user.Uuid] = struct{}{}
}

func deleteUserID(usersByUUID map[string]panel.UserInfo, uuidsByUserID map[int]map[string]struct{}, userID int) {
	if userID <= 0 {
		return
	}
	uuids := uuidsByUserID[userID]
	for uuid := range uuids {
		delete(usersByUUID, uuid)
	}
	delete(uuidsByUserID, userID)
}

func deleteUserUUID(usersByUUID map[string]panel.UserInfo, uuidsByUserID map[int]map[string]struct{}, uuid string) bool {
	user, ok := usersByUUID[uuid]
	if !ok {
		return false
	}
	delete(usersByUUID, uuid)
	if user.Id > 0 {
		if uuids := uuidsByUserID[user.Id]; uuids != nil {
			delete(uuids, uuid)
			if len(uuids) == 0 {
				delete(uuidsByUserID, user.Id)
			}
		}
	}
	return true
}

func userDeltaPruneTime(delta *panel.UserDeltaData) (int64, bool) {
	if delta == nil || delta.ServerTime <= 0 {
		return 0, false
	}
	return delta.ServerTime, true
}

func removeExpiredUsers(users []panel.UserInfo, nowUnix int64) ([]panel.UserInfo, bool) {
	if len(users) == 0 {
		return users, false
	}

	next := make([]panel.UserInfo, 0, len(users))
	changed := false
	for _, user := range users {
		if user.ExpiredAt > 0 && user.ExpiredAt <= nowUnix {
			changed = true
			continue
		}
		next = append(next, user)
	}
	if !changed {
		return users, false
	}
	return next, true
}
