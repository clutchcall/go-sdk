package clutchcall

const (
    ErrorCode_SUCCESS uint32 = 0
    ErrorCode_ERR_INVALID_TRUNK uint32 = 1
    ErrorCode_ERR_INVALID_DESTINATION uint32 = 2
    ErrorCode_ERR_RATE_LIMITED uint32 = 3
    ErrorCode_ERR_CIRCUIT_BREAKER uint32 = 4
    ErrorCode_ERR_INTERNAL_ERROR uint32 = 5
    ErrorCode_ERR_VALIDATION_FAILED uint32 = 6
    ErrorCode_ERR_UNAUTHORIZED uint32 = 7
)

const (
    DialplanAction_HANGUP uint32 = 0
    DialplanAction_PARK uint32 = 1
    DialplanAction_MUSIC_ON_HOLD uint32 = 2
    DialplanAction_PLAYBACK uint32 = 3
    DialplanAction_UNPARK_AND_BRIDGE uint32 = 4
    DialplanAction_ANSWER uint32 = 5
    DialplanAction_AI_BIDIRECTIONAL_STREAM uint32 = 6
    DialplanAction_TRANSFER uint32 = 7
    DialplanAction_MUTE uint32 = 8
    DialplanAction_UNMUTE uint32 = 9
    DialplanAction_HOLD uint32 = 10
    DialplanAction_UNHOLD uint32 = 11
    DialplanAction_SEND_DTMF uint32 = 12
    DialplanAction_SUPERVISE uint32 = 13
    DialplanAction_LOOPBACK uint32 = 14
)

const (
    InboundRule_REJECT uint32 = 0
    InboundRule_PLAY_AND_HANGUP uint32 = 1
    InboundRule_NOTIFY_AND_HANGUP uint32 = 2
    InboundRule_HANDLE_AI uint32 = 3
)

const (
    EventType_UNKNOWN uint32 = 0
    EventType_CHANNEL_CREATE uint32 = 1
    EventType_CHANNEL_ANSWER uint32 = 2
    EventType_CHANNEL_HANGUP_COMPLETE uint32 = 3
    EventType_CHANNEL_HOLD uint32 = 4
    EventType_CHANNEL_RESUME uint32 = 5
)

const (
    MethodID_ORIGINATE uint32 = 1430677891
    MethodID_ORIGINATE_BULK uint32 = 721069100
    MethodID_ABORT_BULK uint32 = 3861915064
    MethodID_TERMINATE uint32 = 3834253405
    MethodID_STREAM_EVENTS uint32 = 959835745
    MethodID_SET_INBOUND_ROUTING uint32 = 1933986897
    MethodID_GET_INCOMING_CALLS uint32 = 1161946746
    MethodID_ANSWER_INCOMING_CALL uint32 = 2990157256
    MethodID_GET_ACTIVE_BUCKETS uint32 = 2624504207
    MethodID_GET_BUCKET_CALLS uint32 = 1217351135
    MethodID_EXECUTE_BUCKET_ACTION uint32 = 4030863293
    MethodID_EXECUTE_DIALPLAN uint32 = 80147304
    MethodID_BARGE uint32 = 3854301714
    MethodID_SUPERVISE_SUBSCRIBE uint32 = 425376200
    MethodID_AUDIO_FRAME uint32 = 2991054320
)
