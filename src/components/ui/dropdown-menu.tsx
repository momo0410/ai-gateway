import * as React from "react";
import * as DropdownMenuPrimitive from "@radix-ui/react-dropdown-menu";
import { Check as CheckIcon } from "lucide-react";

import { cn } from "@/lib/utils";

function DropdownMenu(props: React.ComponentProps<typeof DropdownMenuPrimitive.Root>) {
  return <DropdownMenuPrimitive.Root data-slot="dropdown-menu" {...props} />;
}

function DropdownMenuTrigger(props: React.ComponentProps<typeof DropdownMenuPrimitive.Trigger>) {
  return <DropdownMenuPrimitive.Trigger data-slot="dropdown-menu-trigger" {...props} />;
}

function DropdownMenuContent({ className, sideOffset = 6, ...props }: React.ComponentProps<typeof DropdownMenuPrimitive.Content>) {
  return (
    <DropdownMenuPrimitive.Portal>
      <DropdownMenuPrimitive.Content
        data-slot="dropdown-menu-content"
        sideOffset={sideOffset}
        className={cn(
          "z-50 min-w-36 overflow-hidden rounded-lg border bg-popover p-1.5 text-popover-foreground shadow-[0_10px_30px_rgba(15,23,42,.12)]",
          "data-[state=open]:animate-in data-[state=closed]:animate-out data-[state=closed]:fade-out-0 data-[state=open]:fade-in-0 data-[state=closed]:zoom-out-95 data-[state=open]:zoom-in-95",
          "data-[side=bottom]:slide-in-from-top-1 data-[side=left]:slide-in-from-right-1 data-[side=right]:slide-in-from-left-1 data-[side=top]:slide-in-from-bottom-1",
          className,
        )}
        {...props}
      />
    </DropdownMenuPrimitive.Portal>
  );
}

function DropdownMenuItem({ className, inset, ...props }: React.ComponentProps<typeof DropdownMenuPrimitive.Item> & { inset?: boolean }) {
  return (
    <DropdownMenuPrimitive.Item
      data-slot="dropdown-menu-item"
      data-inset={inset}
      className={cn(
        "relative flex cursor-default select-none items-center gap-2 rounded-md px-2.5 py-2 text-sm outline-none transition-colors",
        "focus:bg-accent focus:text-accent-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50 data-[inset=true]:pl-8 [&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0",
        className,
      )}
      {...props}
    />
  );
}

/**
 * 带勾选框的菜单项（可多选列表用）。
 *
 * 为什么按 UI 规范包一层 Radix 而不是自己拿 DropdownMenuItem + Checkbox 拼：
 * 拼出来的版本会丢掉两件**免费但难做对**的东西 ——
 *  1. `aria-checked` / `role="menuitemcheckbox"` 语义（屏幕阅读器据此报「已勾选」）
 *  2. 菜单打开时键盘可达、Enter/Space 切换、以及 Radix 的 typeahead 首字母跳转
 * 自定义组合要么漏掉其一，要么得把 Radix 内部逻辑再写一遍。
 *
 * 勾选态用 Radix 的 ItemIndicator 而不是自己塞一个图标：它由
 * `data-state=checked` 驱动，与 `checked` prop 天然同步，不需要在调用方维护映射。
 */
function DropdownMenuCheckboxItem({
  className,
  children,
  checked,
  ...props
}: React.ComponentProps<typeof DropdownMenuPrimitive.CheckboxItem>) {
  return (
    <DropdownMenuPrimitive.CheckboxItem
      data-slot="dropdown-menu-checkbox-item"
      checked={checked}
      className={cn(
        "relative flex cursor-default select-none items-center gap-2 rounded-md px-2.5 py-2 text-sm outline-none transition-colors",
        "focus:bg-accent focus:text-accent-foreground data-[disabled]:pointer-events-none data-[disabled]:opacity-50 [&_svg]:pointer-events-none [&_svg]:size-4 [&_svg]:shrink-0",
        className,
      )}
      {...props}
    >
      <span className="flex size-4 shrink-0 items-center justify-center rounded-[4px] border border-input">
        <DropdownMenuPrimitive.ItemIndicator>
          <CheckIcon className="size-3.5" />
        </DropdownMenuPrimitive.ItemIndicator>
      </span>
      {children}
    </DropdownMenuPrimitive.CheckboxItem>
  );
}

function DropdownMenuSeparator({ className, ...props }: React.ComponentProps<typeof DropdownMenuPrimitive.Separator>) {
  return <DropdownMenuPrimitive.Separator data-slot="dropdown-menu-separator" className={cn("-mx-1 my-1 h-px bg-border", className)} {...props} />;
}

/**
 * 菜单分组标题。
 *
 * 为什么需要它：账号菜单里现在既有「只作用于这个号」的动作，也有
 * 「触发一整轮、作用于全部账号」的动作。两者混在一起排成一条竖列时，
 * 用户会默认每一项都只影响当前卡片 —— 点完却动了所有号，属于误导。
 * 分组标题是区分它们最轻的手段（不额外增加点击层级）。
 */
function DropdownMenuLabel({ className, ...props }: React.ComponentProps<typeof DropdownMenuPrimitive.Label>) {
  return (
    <DropdownMenuPrimitive.Label
      data-slot="dropdown-menu-label"
      className={cn("px-2.5 py-1.5 text-[11px] font-semibold text-muted-foreground", className)}
      {...props}
    />
  );
}

export {
  DropdownMenu,
  DropdownMenuCheckboxItem,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
};
