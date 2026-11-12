package download

// The existing filesystem adapters do not provide conditional-owned-unlink.
// A pathname stat followed by unlink is not such an operation. Reject the
// entire force selection before removing membership or any durable Task; the
// ordinary task-only Delete retains outputs and native lifecycle semantics.
func (d *Downloader) checkPathForceDelete(tasks []*Task) error {
 for _,task:=range tasks {
  task.dataLock.RLock()
  declared:=task.PathFreeze!=nil || task.CreationOperation!=nil && task.CreationOperation.Version==2 && task.CreationOperation.Namespace==batchOperationNamespace
  task.dataLock.RUnlock()
  if declared{return ErrPathFreezeHeld}
  var current Task
  found,err:=d.storage.Get(bucketTask,task.ID,&current);if err!=nil{return ErrPathFreezeUnavailable}
  if found&&(current.PathFreeze!=nil||current.CreationOperation!=nil&&current.CreationOperation.Version==2&&current.CreationOperation.Namespace==batchOperationNamespace){return ErrPathFreezeHeld}
  var reverse operationTaskLink
  linked,err:=d.storage.Get(bucketOperationTasks,task.ID,&reverse);if err!=nil{return ErrPathFreezeUnavailable}
  if linked&&reverse.Ref.Version==2&&reverse.Ref.Namespace==batchOperationNamespace{return ErrPathFreezeHeld}
 }
 return nil
}
