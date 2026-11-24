package gateway

// Stages a note for the next turn of every open chat of a profile, installed by the gateway runner that serves them, nil while no runner serves chats in this process
var StageProfileTurnNote func(profileKey, note string)
