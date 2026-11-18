package tuigateway

import "github.com/nickheyer/icebath/pkg/internal/pydantic"

var LearningGraphParams = pydantic.NewModel("LearningGraphParams", ParamsConfig)

var LearningGraphResult = pydantic.NewModel("LearningGraphResult", ClosedConfig,
	pydantic.Req("nodes", pydantic.List(pydantic.Dict(pydantic.Any))),
	pydantic.Req("edges", pydantic.List(pydantic.Dict(pydantic.Any))),
	pydantic.Req("clusters", pydantic.List(pydantic.Dict(pydantic.Any))),
	pydantic.Req("memory", pydantic.List(pydantic.Dict(pydantic.Any))),
	pydantic.Req("stats", pydantic.Dict(pydantic.Any)),
).Described("``agent.learning_graph.build_learning_graph`` payload the /journey view renders at any reveal.")

var _ = Method("learning.graph", LearningGraphParams, LearningGraphResult, "The profile's learning graph, the skill and memory nodes with their links and stats.")
