package irc

// Command name constants matching the Command field of a parsed Message.
// Commands are case-insensitive on the wire but conventionally upper-cased;
// these constants are the canonical upper-case forms the client emits and
// compares against (callers should fold received commands as needed).
const (
	CAP          = "CAP"
	AUTHENTICATE = "AUTHENTICATE"
	PASS         = "PASS"
	NICK         = "NICK"
	USER         = "USER"
	PING         = "PING"
	PONG         = "PONG"
	PRIVMSG      = "PRIVMSG"
	NOTICE       = "NOTICE"
	JOIN         = "JOIN"
	PART         = "PART"
	QUIT         = "QUIT"
	MODE         = "MODE"
	NAMES        = "NAMES"
	WHOIS        = "WHOIS"
	KICK         = "KICK"
	INVITE       = "INVITE"
	TOPIC        = "TOPIC"
	BATCH        = "BATCH"
	TAGMSG       = "TAGMSG"
	ACCOUNT      = "ACCOUNT"
	AWAY         = "AWAY"
	CHGHOST      = "CHGHOST"
	SETNAME      = "SETNAME"
)

// CAP subcommands, carried as the first/second parameter of a CAP message.
const (
	CAP_LS   = "LS"
	CAP_REQ  = "REQ"
	CAP_ACK  = "ACK"
	CAP_NAK  = "NAK"
	CAP_NEW  = "NEW"
	CAP_DEL  = "DEL"
	CAP_END  = "END"
	CAP_LIST = "LIST"
)
