package dashboard

var delegationBackendSelectModel = &Model{ClassName: "DelegationBackendSelect", Fields: []ModelField{
	Field("backend", Str),
	FieldDefault("profile", Optional(Str), nil),
}}

var delegationModelMapUpdateModel = &Model{ClassName: "DelegationModelMapUpdate", Fields: []ModelField{
	Field("options", ListOf(DictOf(Any))),
	FieldDefault("default", Optional(Str), nil),
	FieldDefault("profile", Optional(Str), nil),
}}

var delegationProfileRoutesUpdateModel = &Model{ClassName: "DelegationProfileRoutesUpdate", Fields: []ModelField{
	Field("profiles", ListOf(DictOf(Any))),
	FieldDefault("default", Optional(Str), nil),
	FieldDefault("profile", Optional(Str), nil),
}}
