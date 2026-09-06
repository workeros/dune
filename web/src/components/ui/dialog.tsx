import * as React from "react";
import * as DialogPrimitive from "@radix-ui/react-dialog";
import { Icon } from "@iconify/react";
import closeLine from "@iconify-icons/ri/close-line";
import { cn } from "@/lib/utils";
export const Dialog = DialogPrimitive.Root;
export const DialogTrigger = DialogPrimitive.Trigger;
export const DialogTitle = DialogPrimitive.Title;
export const DialogDescription = DialogPrimitive.Description;
export const DialogClose = DialogPrimitive.Close;
export function DialogContent({ className, children, ...props }: React.ComponentProps<typeof DialogPrimitive.Content>) {
  return <DialogPrimitive.Portal><DialogPrimitive.Overlay className="fixed inset-0 z-40 bg-foreground/25 backdrop-blur-xs" /><DialogPrimitive.Content className={cn("fixed top-1/2 left-1/2 z-50 max-h-[90vh] w-[calc(100%-2rem)] max-w-xl -translate-x-1/2 -translate-y-1/2 overflow-y-auto rounded-xl border-2 border-foreground bg-card p-6 shadow-[6px_6px_0_var(--ink)]", className)} {...props}>{children}<DialogPrimitive.Close className="absolute top-4 right-4 rounded p-1 focus-visible:outline-2 focus-visible:outline-ring" aria-label="关闭"><Icon icon={closeLine} width={20} /></DialogPrimitive.Close></DialogPrimitive.Content></DialogPrimitive.Portal>;
}
