// backend group bridge, its Go service BackendService lands with the backend lane
import * as BackendService from '../bindings/github.com/nickheyer/icebath/apps/desktop/backendservice.js'
import { fire } from '../runtime'

export const getConnection = (profile, opts) => BackendService.GetConnection(profile, opts)
export const revalidateConnection = () => BackendService.RevalidateConnection()
export const getGatewayWsUrl = (profile) => BackendService.GetGatewayWsUrl(profile)
export const getBootProgress = () => BackendService.GetBootProgress()
export const setActiveConnectionRoute = (route) => fire(BackendService.SetActiveConnectionRoute(route))
export const probeLocalBackend = () => BackendService.ProbeLocalBackend()
export const recycleBackend = (profile) => BackendService.RecycleBackend(profile)
