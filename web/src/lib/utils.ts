import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// 引き出し/ダイアログ(Sheet/Dialog)の onInteractOutside で閉じるかを判定する補助。
//
// 背景:引き出しの中の Radix の重なった層(Select のプルダウン、DropdownMenu、Popover など)は引き出しの
// 外に portal される。重なった層が開いているときにオーバーレイ/引き出しの外を押してそれを閉じたいと、その pointerdown は Select と Sheet の2つの
// DismissableLayer が同時に扱う;Select が先に閉じ、それは discrete のイベントなので React が同期で flush し、そのため
// Sheet の処理の番が来たときには重なった層の data-state はもう closed に変わっている —— 「その場で」重なった層が開いているかを調べるのは
// もともと当てにならない(実際に確かめた)。
//
// 正しいやり方:Radix の pointerdown の受け手はバブリングの段階にある;我々は capture の段階(それより早い)で先に
// 「いま重なった層が開いているか」を記録し、onInteractOutside がその記録した値を読んで閉じてよいかを決める。
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture:Radix のバブリングの段階の pointerdown の処理より先に記録する
  );
}

// radixOverlayWasOpenAtPointerDown は「最後の pointerdown が起きたときに Radix の重なった層が
// 開いていたか」を返す。引き出し/ダイアログはそれに従う:重なった層が開いているときにオーバーレイを押す → 重なった層だけを閉じ、自分は閉じない。
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText は文字をクリップボードに書き込み、成功したかを返す。
// 背景:navigator.clipboard は安全な文脈(HTTPS / localhost)でしか使えない;IP + HTTP で
// 開いたときは undefined なので、そのときは execCommand("copy") に落とす。
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // 代わりの方法に進む
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "en-US", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
