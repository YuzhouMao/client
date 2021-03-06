// @flow
import * as React from 'react'
import * as Kb from '../../../common-adapters'
import * as Styles from '../../../styles'

const CaptionedButton = (props: {
  label: string,
  caption: string,
  onClick: () => void,
  style?: Styles.StylesCrossPlatform,
  waitOnClick?: boolean,
}) => (
  <Kb.Box2
    direction="vertical"
    style={Styles.collapseStyles([Styles.globalStyles.flexBoxColumn, props.style])}
    gap="tiny"
  >
    {props.waitOnClick ? (
      <Kb.WaitingButton
        type="Primary"
        small={true}
        label={props.label}
        onClick={props.onClick}
        waitingKey={null}
      />
    ) : (
      <Kb.Button type="Primary" small={true} label={props.label} onClick={props.onClick} />
    )}
    <Kb.Text center={true} type="BodySmall">
      {props.caption}
    </Kb.Text>
  </Kb.Box2>
)

const DangerButton = (props: {label: string, onClick: () => void}) => (
  <Kb.ButtonBar small={true}>
    <Kb.Button type="Danger" small={true} label={props.label} onClick={props.onClick} />
  </Kb.ButtonBar>
)

const CaptionedDangerIcon = ({
  icon,
  caption,
  onClick,
}: {
  icon: Kb.IconType,
  caption: string,
  onClick: () => void,
}) => (
  <Kb.ClickableBox
    style={{
      ...Styles.globalStyles.flexBoxRow,
      alignItems: 'center',
      justifyContent: 'center',
      paddingBottom: Styles.globalMargins.tiny,
      paddingTop: Styles.globalMargins.tiny,
    }}
    onClick={onClick}
  >
    <Kb.Icon type={icon} style={{marginRight: Styles.globalMargins.tiny}} color={Styles.globalColors.red} />
    <Kb.Text type="BodySemibold" style={{color: Styles.globalColors.red}} className="hover-underline">
      {caption}
    </Kb.Text>
  </Kb.ClickableBox>
)

export {CaptionedButton, DangerButton, CaptionedDangerIcon}
