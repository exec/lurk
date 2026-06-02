package irc

// Numeric reply codes, as three-character strings matching the Command field
// of a parsed Message. Names follow the conventional RFC/IRCv3 symbolic names.
// This is not an exhaustive list of IRC numerics — only those the client
// currently acts on, plus the common registration burst.
const (
	RPL_WELCOME  = "001" // welcome line; registration is complete
	RPL_YOURHOST = "002"
	RPL_CREATED  = "003"
	RPL_MYINFO   = "004"
	RPL_ISUPPORT = "005" // server feature tokens (parsed by the isupport package)

	RPL_NOTOPIC      = "331" // <client> <channel> :No topic is set
	RPL_TOPIC        = "332" // <client> <channel> :<topic>
	RPL_TOPICWHOTIME = "333" // <client> <channel> <setBy> <setAt unix-seconds>

	RPL_NAMREPLY   = "353" // channel member list (NAMES)
	RPL_ENDOFNAMES = "366"

	// WHOIS reply burst: 311 opens, 318 closes; the rest are optional detail
	// lines a client renders as the whois result.
	RPL_AWAY          = "301" // <client> <nick> :<away message>
	RPL_WHOISUSER     = "311" // <client> <nick> <user> <host> * :<realname>
	RPL_WHOISSERVER   = "312" // <client> <nick> <server> :<server info>
	RPL_WHOISOPERATOR = "313"
	RPL_WHOISIDLE     = "317" // <client> <nick> <idle secs> [<signon>] :seconds idle
	RPL_ENDOFWHOIS    = "318"
	RPL_WHOISCHANNELS = "319" // <client> <nick> :[prefix]<channel> ...
	RPL_WHOISACCOUNT  = "330" // <client> <nick> <account> :is logged in as
	RPL_WHOISACTUALLY = "338"
	RPL_WHOISSECURE   = "671" // <client> <nick> :is using a secure connection

	RPL_MOTDSTART = "375"
	RPL_MOTD      = "372"
	RPL_ENDOFMOTD = "376"
	ERR_NOMOTD    = "422"

	ERR_NONICKNAMEGIVEN  = "431"
	ERR_ERRONEUSNICKNAME = "432"
	ERR_NICKNAMEINUSE    = "433"

	ERR_NOTREGISTERED     = "451"
	ERR_NEEDMOREPARAMS    = "461"
	ERR_ALREADYREGISTERED = "462"

	ERR_INPUTTOOLONG = "417" // line exceeded the server's length budget

	// SASL numerics (sasl-3.1 / 3.2).
	RPL_LOGGEDIN    = "900"
	RPL_LOGGEDOUT   = "901"
	ERR_NICKLOCKED  = "902"
	RPL_SASLSUCCESS = "903"
	ERR_SASLFAIL    = "904"
	ERR_SASLTOOLONG = "905"
	ERR_SASLABORTED = "906"
	ERR_SASLALREADY = "907"
	RPL_SASLMECHS   = "908"
)
